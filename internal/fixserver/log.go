package fixserver

import (
	"bytes"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/quickfixgo/quickfix"

	"github.com/quynv/mock-fixser/internal/metrics"
)

// Tap receives every raw FIX message a session sends ('O') or receives
// ('I'), exactly as on the wire. The monitor mirror (U1) uses it.
type Tap func(id quickfix.SessionID, dir byte, raw []byte)

// slogLogFactory routes quickfixgo session events to slog at debug level,
// counts messages and feeds raw messages to the tap.
type slogLogFactory struct {
	log *slog.Logger
	tap *atomic.Pointer[Tap]
}

func (f slogLogFactory) Create() (quickfix.Log, error) {
	return slogLog{log: f.log.With("component", "quickfix")}, nil
}

func (f slogLogFactory) CreateSessionLog(id quickfix.SessionID) (quickfix.Log, error) {
	return slogLog{log: f.log.With("component", "quickfix", "session", id.String()), id: id, session: id.String(), tap: f.tap}, nil
}

type slogLog struct {
	log     *slog.Logger
	id      quickfix.SessionID
	session string
	tap     *atomic.Pointer[Tap]
}

func (l slogLog) OnIncoming(raw []byte) { l.message('I', "in", raw) }
func (l slogLog) OnOutgoing(raw []byte) { l.message('O', "out", raw) }

func (l slogLog) message(dir byte, direction string, raw []byte) {
	if l.tap == nil {
		return // global log: no session
	}
	metrics.FIXMessages.WithLabelValues(l.session, direction, MsgType(raw)).Inc()
	if t := l.tap.Load(); t != nil {
		(*t)(l.id, dir, raw)
	}
}

func (l slogLog) OnEvent(s string)                    { l.log.Debug(s) }
func (l slogLog) OnEventf(format string, args ...any) { l.log.Debug(fmt.Sprintf(format, args...)) }

// MsgType extracts tag 35 from a raw message without parsing it.
func MsgType(raw []byte) string {
	_, rest, ok := bytes.Cut(raw, []byte("\x0135="))
	if !ok {
		return ""
	}
	v, _, _ := bytes.Cut(rest, []byte{0x01})
	return string(v)
}
