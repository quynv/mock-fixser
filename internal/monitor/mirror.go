// Package monitor mirrors raw FIX traffic to the UI_MONITOR session as U1
// MirrorLog messages and handles U2 MonitorControl (design doc, section 8.1).
package monitor

import (
	"bytes"
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/quickfixgo/quickfix"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/mirrorlog"
	"github.com/quynv/mock-fixser/fix/fix44/monitorcontrol"
	"github.com/quynv/mock-fixser/internal/clock"
	"github.com/quynv/mock-fixser/internal/config"
	"github.com/quynv/mock-fixser/internal/fixserver"
	"github.com/quynv/mock-fixser/internal/metrics"
)

// Sender sends one message to a session (quickfix.SendToTarget).
type Sender func(m *quickfix.Message, id quickfix.SessionID) error

type entry struct {
	session quickfix.SessionID
	dir     byte
	raw     []byte
	at      time.Time
}

type settings struct {
	enabled bool // monitor.enabled && monitor.mirror_log
	monitor quickfix.SessionID
	exclude map[string]bool
}

// Mirror copies every client message into a bounded buffer and sends it to
// the monitor session from its own goroutine. Capture never blocks: when
// the buffer is full the message is dropped and counted, so a slow UI
// never slows down trading (section 2.2).
type Mirror struct {
	clock clock.Clock
	log   *slog.Logger
	send  Sender

	settings atomic.Pointer[settings]
	// deliver is turned on by U2 log=on from the monitor session; it is off
	// by default to save bandwidth (section 8.3).
	deliver atomic.Bool
	dropped atomic.Uint64

	ch chan entry
}

// New sizes the buffer from monitor.mirror_buffer; a later change of that
// value needs a restart.
func New(cfg *config.Config, clk clock.Clock, log *slog.Logger, send Sender) *Mirror {
	if send == nil {
		send = func(m *quickfix.Message, id quickfix.SessionID) error { return quickfix.SendToTarget(m, id) }
	}
	m := &Mirror{clock: clk, log: log, send: send, ch: make(chan entry, max(cfg.Server.Monitor.MirrorBuffer, 1))}
	m.SetConfig(cfg)
	return m
}

func (m *Mirror) SetConfig(cfg *config.Config) {
	mon := cfg.Server.Monitor
	s := &settings{
		enabled: mon.Enabled && mon.MirrorLog,
		monitor: quickfix.SessionID{BeginString: cfg.Server.FIX.Version, SenderCompID: mon.SenderCompID, TargetCompID: mon.TargetCompID},
		exclude: map[string]bool{},
	}
	for _, t := range mon.MirrorExclude {
		s.exclude[t] = true
	}
	m.settings.Store(s)
}

// Delivering reports whether U1 delivery is on.
func (m *Mirror) Delivering() bool { return m.deliver.Load() }

// Dropped returns how many messages were dropped because the buffer was full.
func (m *Mirror) Dropped() uint64 { return m.dropped.Load() }

// Capture is the fixserver tap. Messages of the monitor session itself are
// never mirrored, which would loop.
func (m *Mirror) Capture(id quickfix.SessionID, dir byte, raw []byte) {
	s := m.settings.Load()
	if !s.enabled || !m.deliver.Load() || id == s.monitor {
		return
	}
	if s.exclude[fixserver.MsgType(raw)] {
		return
	}
	e := entry{session: id, dir: dir, raw: bytes.Clone(raw), at: m.clock.Now()}
	select {
	case m.ch <- e:
	default:
		m.dropped.Add(1)
		metrics.MirrorDropped.Inc()
	}
}

// Run sends buffered messages as U1 until ctx ends.
func (m *Mirror) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-m.ch:
			s := m.settings.Load()
			if !s.enabled || !m.deliver.Load() {
				continue
			}
			if err := m.send(mirrorMsg(e), s.monitor); err != nil {
				m.log.Debug("mirror send failed", "error", err)
			}
		}
	}
}

// Route registers the U2 handler.
func (m *Mirror) Route(r *quickfix.MessageRouter) {
	r.AddRoute(monitorcontrol.Route(m.HandleControl))
}

// HandleControl processes U2 from the monitor session. Supported:
// log=on|off turns U1 delivery on or off.
func (m *Mirror) HandleControl(msg monitorcontrol.MonitorControl, id quickfix.SessionID) quickfix.MessageRejectError {
	if id != m.settings.Load().monitor {
		return quickfix.NewBusinessMessageRejectError("MonitorControl is accepted from the monitor session only", 4, nil)
	}
	key, err := msg.GetControlKey()
	if err != nil {
		return err
	}
	val, err := msg.GetControlValue()
	if err != nil {
		return err
	}
	switch {
	case key == "log" && (val == "on" || val == "off"):
		m.deliver.Store(val == "on")
		m.log.Info("mirror delivery", "on", val == "on")
		return nil
	}
	return quickfix.NewBusinessMessageRejectError("Unsupported MonitorControl "+key+"="+val, 0, nil)
}

// OnLogout turns delivery off when the monitor session goes away.
func (m *Mirror) OnLogout(id quickfix.SessionID) {
	if id == m.settings.Load().monitor {
		m.deliver.Store(false)
	}
}

func mirrorMsg(e entry) *quickfix.Message {
	dir := enum.MirrorDirection_INBOUND
	if e.dir == 'O' {
		dir = enum.MirrorDirection_OUTBOUND
	}
	return mirrorlog.New(
		field.NewMirrorSessionID(e.session.String()),
		field.NewMirrorDirection(dir),
		field.NewMirrorRawMsg(string(bytes.ReplaceAll(e.raw, []byte{0x01}, []byte{'|'}))),
		field.NewMirrorTimeWithPrecision(e.at, quickfix.Micros),
	).ToMessage()
}
