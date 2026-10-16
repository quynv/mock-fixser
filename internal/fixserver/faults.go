package fixserver

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/quickfixgo/quickfix"

	"github.com/quynv/mock-fixser/internal/config"
)

// Session fault simulation (design doc, section 5.4). Configured faults
// start at each logon; the admin API can also trigger one at once.

// Fault kinds for TriggerFault.
const (
	FaultDrop          = "drop"
	FaultSeqGap        = "seq_gap"
	FaultHeartbeatMiss = "heartbeat_miss"
)

// ErrUnknownFault is returned by TriggerFault for an unsupported kind.
var ErrUnknownFault = errors.New("unknown fault (use drop, seq_gap or heartbeat_miss)")

// faultListener wraps accepted connections so heartbeats can be dropped.
type faultListener struct{ net.Listener }

func (l faultListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &faultConn{Conn: c}, nil
}

func newFaultListener(address string, tlsConfig *tls.Config) (net.Listener, error) {
	var l net.Listener
	var err error
	if tlsConfig != nil {
		l, err = tls.Listen("tcp", address, tlsConfig)
	} else {
		l, err = net.Listen("tcp", address)
	}
	if err != nil {
		return nil, err
	}
	return faultListener{l}, nil
}

// faultConn drops outgoing heartbeats while dropHeartbeats is set, and
// reports completed writes to the session's flow control. quickfixgo
// writes one whole message per Write call.
type faultConn struct {
	net.Conn
	dropHeartbeats atomic.Bool
	flow           atomic.Pointer[flow]
}

var heartbeatTag = []byte("\x0135=0\x01")

func (c *faultConn) Write(b []byte) (int, error) {
	if c.dropHeartbeats.Load() && bytes.Contains(b, heartbeatTag) {
		return len(b), nil
	}
	n, err := c.Conn.Write(b)
	if f := c.flow.Load(); f != nil {
		f.written(b)
	}
	return n, err
}

// faultState is the running fault simulation of one logged-on session.
type faultState struct {
	faults *config.SessionFaults
	conn   *faultConn
	msgs   int // messages in both directions since logon
	sent   int // application messages sent since logon
	closed bool
	timers []*time.Timer
}

func (s *Server) faultsFor(id quickfix.SessionID) *config.SessionFaults {
	return (*s.faults.Load())[id]
}

// checkLogon rejects Logon when reject_logon is configured.
func (s *Server) checkLogon(msg *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
	if mt, _ := msg.MsgType(); mt != "A" {
		return nil
	}
	if f := s.faultsFor(id); f != nil && f.RejectLogon != nil {
		text := f.RejectLogon.Text
		if text == "" {
			text = "Logon rejected (simulated fault)"
		}
		s.log.Info("fault: logon rejected", "session", id.String())
		return quickfix.RejectLogon{Text: text}
	}
	return nil
}

// startFaults begins the configured faults of a session at logon.
func (s *Server) startFaults(id quickfix.SessionID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	conn, _ := s.conns[id].(*faultConn)
	st := &faultState{faults: s.faultsFor(id), conn: conn}
	s.faultStates[id] = st
	f := st.faults
	if f == nil || conn == nil {
		return
	}
	if f.DropAfter != nil && f.DropAfter.Seconds > 0 {
		st.timers = append(st.timers, time.AfterFunc(time.Duration(f.DropAfter.Seconds)*time.Second, func() {
			s.log.Info("fault: connection dropped", "session", id.String(), "after_seconds", f.DropAfter.Seconds)
			_ = conn.Close()
		}))
	}
	if f.HeartbeatMiss != nil {
		st.timers = append(st.timers, time.AfterFunc(time.Duration(f.HeartbeatMiss.AfterSeconds)*time.Second, func() {
			s.log.Info("fault: heartbeats stopped", "session", id.String())
			conn.dropHeartbeats.Store(true)
		}))
	}
}

func (s *Server) stopFaults(id quickfix.SessionID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.faultStates[id]; st != nil {
		for _, t := range st.timers {
			t.Stop()
		}
		delete(s.faultStates, id)
	}
}

// observe counts the messages of a session for drop_after and seq_gap. It
// runs on the sending or receiving goroutine and never blocks.
func (s *Server) observe(id quickfix.SessionID, dir byte, raw []byte) {
	s.mu.Lock()
	st := s.faultStates[id]
	if st == nil || st.faults == nil {
		s.mu.Unlock()
		return
	}
	f := st.faults
	st.msgs++
	drop := f.DropAfter != nil && f.DropAfter.Messages > 0 && st.msgs >= f.DropAfter.Messages && !st.closed
	if drop {
		st.closed = true
	}
	gap := false
	// Only application messages (not resends) count, so the gap fill that
	// answers the ResendRequest cannot start another gap.
	if dir == 'O' && f.SeqGap != nil && !isAdmin(MsgType(raw)) && !bytes.Contains(raw, []byte("\x0143=Y\x01")) {
		st.sent++
		gap = st.sent%f.SeqGap.Every == 0
	}
	conn := st.conn
	s.mu.Unlock()

	if gap {
		s.skipSeq(id)
	}
	if drop && conn != nil {
		s.log.Info("fault: connection dropped", "session", id.String(), "after_messages", f.DropAfter.Messages)
		go conn.Close() // not from inside quickfixgo's read or write
	}
}

func isAdmin(msgType string) bool {
	switch msgType {
	case "0", "1", "2", "3", "4", "5", "A":
		return true
	}
	return false
}

// skipSeq makes the next outgoing message skip one MsgSeqNum.
func (s *Server) skipSeq(id quickfix.SessionID) {
	st, err := s.stores.Create(id)
	if err != nil {
		return
	}
	if err := st.IncrNextSenderMsgSeqNum(); err != nil {
		s.log.Warn("seq_gap failed", "session", id.String(), "error", err)
	}
}

// TriggerFault applies a fault to a logged-on client session at once:
// drop closes the connection, seq_gap skips one MsgSeqNum, heartbeat_miss
// stops heartbeats until the session reconnects.
func (s *Server) TriggerFault(id quickfix.SessionID, kind string) error {
	if !s.app.loggedOn(id) {
		return ErrNotLoggedOn
	}
	s.mu.Lock()
	conn, _ := s.conns[id].(*faultConn)
	s.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("no connection for %s", id)
	}
	s.log.Info("fault triggered", "session", id.String(), "fault", kind)
	switch kind {
	case FaultDrop:
		return conn.Close()
	case FaultSeqGap:
		s.skipSeq(id)
		return nil
	case FaultHeartbeatMiss:
		conn.dropHeartbeats.Store(true)
		return nil
	}
	return ErrUnknownFault
}
