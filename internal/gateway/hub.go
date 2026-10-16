// Package gateway connects to the monitor acceptor as UI_MONITOR and pipes
// raw FIX between that session and browsers over WebSocket (design doc,
// sections 8.2 and 8.3).
package gateway

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/quickfixgo/quickfix"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/marketdatarequest"
	"github.com/quynv/mock-fixser/fix/fix44/monitorcontrol"
)

// Client is one browser connection. Frames to it go through a bounded
// channel; a client that falls behind is kicked (closed with 1008) instead
// of blocking the hub.
type Client struct {
	out  chan string
	kick func()

	// Guarded by Hub.mu.
	reqs    map[string][]string // client MDReqID -> symbols
	wantLog bool
	dead    bool
}

// Frames returns the frames to write to the browser.
func (c *Client) Frames() <-chan string { return c.out }

// symSub is the one gateway subscription shared by every client viewing a
// symbol.
type symSub struct {
	symbol  string
	gwReqID string
	refs    map[*Client]string // client -> its MDReqID for this symbol
	ring    []string           // latest W/X frames, with the gateway MDReqID
}

// Hub fans FIX messages from the monitor session out to browser clients
// and turns browser requests into shared upstream requests.
type Hub struct {
	log     *slog.Logger
	send    func(*quickfix.Message) error // to the server over UI_MONITOR
	history int
	buffer  int

	mu      sync.Mutex
	clients map[*Client]struct{}
	subs    map[string]*symSub // by symbol
	byGW    map[string]*symSub // by gateway MDReqID
	seq     int
	online  bool // UI_MONITOR logged on
	logOn   bool // U1 delivery state requested upstream
}

func NewHub(send func(*quickfix.Message) error, history, buffer int, log *slog.Logger) *Hub {
	return &Hub{
		log: log, send: send, history: max(history, 1), buffer: max(buffer, 1),
		clients: map[*Client]struct{}{}, subs: map[string]*symSub{}, byGW: map[string]*symSub{},
	}
}

// NewClient creates a client; kick is called (once) if it falls behind.
func (h *Hub) NewClient(kick func()) *Client {
	c := &Client{out: make(chan string, h.buffer), kick: kick, reqs: map[string][]string{}}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	return c
}

// Unregister drops a client and every subscription only it held.
func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	var out []*quickfix.Message
	for id := range c.reqs {
		out = append(out, h.unsubscribeLocked(c, id)...)
	}
	c.wantLog = false
	out = append(out, h.syncLogLocked()...)
	h.mu.Unlock()
	h.sendAll(out)
}

func (h *Hub) sendAll(msgs []*quickfix.Message) {
	for _, m := range msgs {
		if err := h.send(m); err != nil {
			h.log.Debug("gateway send failed", "error", err)
		}
	}
}

// deliverLocked queues a frame without blocking.
func (h *Hub) deliverLocked(c *Client, frame string) {
	if c.dead {
		return
	}
	select {
	case c.out <- frame:
	default:
		c.dead = true
		h.log.Warn("websocket client too slow, closing")
		go c.kick()
	}
}

// HandleFrame processes one frame from a browser: V (subscribe or
// unsubscribe) or U2 (log on/off). Anything else is answered with a
// BusinessMessageReject frame; the browser cannot send orders.
func (h *Hub) HandleFrame(c *Client, frame string) {
	f := parseFrame(frame)
	h.mu.Lock()
	var out []*quickfix.Message
	switch f.get("35") {
	case "V":
		id := f.get("262")
		switch f.get("263") {
		case "1":
			if id == "" || len(f.all("55")) == 0 {
				h.rejectLocked(c, "V", "V needs 262 and at least one 55")
				break
			}
			out = h.subscribeLocked(c, id, f.all("55"))
		case "2":
			out = h.unsubscribeLocked(c, id)
		default:
			h.rejectLocked(c, "V", "only SubscriptionRequestType 1 and 2 are supported")
		}
	case "U2":
		if f.get("5005") != "log" || (f.get("5006") != "on" && f.get("5006") != "off") {
			h.rejectLocked(c, "U2", "supported: 5005=log, 5006=on|off")
			break
		}
		c.wantLog = f.get("5006") == "on"
		out = h.syncLogLocked()
	default:
		h.rejectLocked(c, f.get("35"), "only V and U2 are accepted from the browser")
	}
	h.mu.Unlock()
	h.sendAll(out)
}

func (h *Hub) rejectLocked(c *Client, refMsgType, text string) {
	h.deliverLocked(c, fmt.Sprintf("35=j|372=%s|380=3|58=%s", refMsgType, text))
}

func (h *Hub) subscribeLocked(c *Client, id string, symbols []string) []*quickfix.Message {
	var out []*quickfix.Message
	if _, ok := c.reqs[id]; ok { // re-subscribe with the same id
		out = append(out, h.unsubscribeLocked(c, id)...)
	}
	c.reqs[id] = symbols
	for _, sym := range symbols {
		s, ok := h.subs[sym]
		if !ok {
			h.seq++
			s = &symSub{symbol: sym, gwReqID: fmt.Sprintf("gw-%d-%s", h.seq, sym), refs: map[*Client]string{}}
			h.subs[sym], h.byGW[s.gwReqID] = s, s
			if h.online {
				out = append(out, mdRequest(s.gwReqID, enum.SubscriptionRequestType_SNAPSHOTUPDATE, sym))
			}
		}
		s.refs[c] = id
		// The shared subscription is already running: replay its history so
		// the chart has data at once.
		for _, fr := range s.ring {
			h.deliverLocked(c, rewrite(fr, s.gwReqID, id))
		}
	}
	return out
}

func (h *Hub) unsubscribeLocked(c *Client, id string) []*quickfix.Message {
	var out []*quickfix.Message
	for _, sym := range c.reqs[id] {
		s, ok := h.subs[sym]
		if !ok || s.refs[c] != id {
			continue
		}
		delete(s.refs, c)
		if len(s.refs) == 0 {
			delete(h.subs, sym)
			delete(h.byGW, s.gwReqID)
			if h.online {
				out = append(out, mdRequest(s.gwReqID, enum.SubscriptionRequestType_UNSUBSCRIBE, sym))
			}
		}
	}
	delete(c.reqs, id)
	return out
}

// syncLogLocked sends U2 when the wanted U1 state changes: on while any
// client wants the log.
func (h *Hub) syncLogLocked() []*quickfix.Message {
	want := false
	for c := range h.clients {
		if c.wantLog {
			want = true
			break
		}
	}
	if want == h.logOn || !h.online {
		h.logOn = want && h.online
		return nil
	}
	h.logOn = want
	return []*quickfix.Message{logControl(want)}
}

// OnLogon re-creates every shared subscription and the log state after
// UI_MONITOR logs on (the server drops them at logout).
func (h *Hub) OnLogon() {
	h.mu.Lock()
	h.online = true
	var out []*quickfix.Message
	for _, s := range h.subs {
		s.ring = nil
		out = append(out, mdRequest(s.gwReqID, enum.SubscriptionRequestType_SNAPSHOTUPDATE, s.symbol))
	}
	h.logOn = false
	out = append(out, h.syncLogLocked()...)
	h.mu.Unlock()
	h.sendAll(out)
}

func (h *Hub) OnLogout() {
	h.mu.Lock()
	h.online, h.logOn = false, false
	h.mu.Unlock()
}

// OnServerMessage dispatches an application message from the server: W, X
// and Y to the clients of that subscription, 8 and N (drop copies) to
// every client, U1 to clients that turned the log on.
func (h *Hub) OnServerMessage(m *quickfix.Message) {
	mt, _ := m.MsgType()
	frame := strings.ReplaceAll(m.String(), "\x01", "|")
	h.mu.Lock()
	defer h.mu.Unlock()
	switch mt {
	case "W", "X", "Y":
		gw, _ := m.Body.GetString(262)
		s, ok := h.byGW[gw]
		if !ok {
			return
		}
		if mt == "Y" {
			for c, id := range s.refs {
				h.deliverLocked(c, rewrite(frame, gw, id))
			}
			delete(h.subs, s.symbol)
			delete(h.byGW, gw)
			return
		}
		s.ring = append(s.ring, frame)
		if over := len(s.ring) - h.history; over > 0 {
			s.ring = append(s.ring[:0:0], s.ring[over:]...)
		}
		for c, id := range s.refs {
			h.deliverLocked(c, rewrite(frame, gw, id))
		}
	case "8", "N":
		for c := range h.clients {
			h.deliverLocked(c, frame)
		}
	case "U1":
		for c := range h.clients {
			if c.wantLog {
				h.deliverLocked(c, frame)
			}
		}
	}
}

// rewrite replaces the gateway MDReqID with the client's.
func rewrite(frame, gw, id string) string {
	return strings.Replace(frame, "|262="+gw+"|", "|262="+id+"|", 1)
}

func mdRequest(id string, typ enum.SubscriptionRequestType, symbol string) *quickfix.Message {
	m := marketdatarequest.New(field.NewMDReqID(id), field.NewSubscriptionRequestType(typ), field.NewMarketDepth(0))
	m.SetMDUpdateType(enum.MDUpdateType_INCREMENTAL)
	types := marketdatarequest.NewNoMDEntryTypesRepeatingGroup()
	types.Add().SetMDEntryType(enum.MDEntryType_BID)
	types.Add().SetMDEntryType(enum.MDEntryType_OFFER)
	m.SetNoMDEntryTypes(types)
	syms := marketdatarequest.NewNoRelatedSymRepeatingGroup()
	syms.Add().SetSymbol(symbol)
	m.SetNoRelatedSym(syms)
	return m.ToMessage()
}

func logControl(on bool) *quickfix.Message {
	v := "off"
	if on {
		v = "on"
	}
	return monitorcontrol.New(field.NewControlKey("log"), field.NewControlValue(v)).ToMessage()
}

// frame is a parsed browser frame: tag=value pairs separated by |.
type frame [][2]string

func parseFrame(s string) frame {
	var f frame
	for part := range strings.SplitSeq(strings.Trim(s, "|"), "|") {
		if tag, val, ok := strings.Cut(part, "="); ok {
			f = append(f, [2]string{tag, val})
		}
	}
	return f
}

func (f frame) get(tag string) string {
	for _, p := range f {
		if p[0] == tag {
			return p[1]
		}
	}
	return ""
}

func (f frame) all(tag string) []string {
	var out []string
	for _, p := range f {
		if p[0] == tag {
			out = append(out, p[1])
		}
	}
	return out
}
