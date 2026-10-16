package gateway

import (
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"
	"github.com/shopspring/decimal"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/executionreport"
	"github.com/quynv/mock-fixser/fix/fix44/marketdataincrementalrefresh"
	"github.com/quynv/mock-fixser/fix/fix44/marketdatarequest"
	"github.com/quynv/mock-fixser/fix/fix44/marketdatarequestreject"
	"github.com/quynv/mock-fixser/fix/fix44/marketdatasnapshotfullrefresh"
	"github.com/quynv/mock-fixser/fix/fix44/mirrorlog"
)

type upstream struct {
	mu   sync.Mutex
	sent []string // "V:263:262:55" or "U2:on"
}

func (u *upstream) send(m *quickfix.Message) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	mt, _ := m.MsgType()
	switch mt {
	case "V":
		typ, _ := m.Body.GetString(263)
		id, _ := m.Body.GetString(262)
		var sym string
		if g, err := marketdatarequest.FromMessage(m).GetNoRelatedSym(); err == nil && g.Len() > 0 {
			sym, _ = g.Get(0).GetSymbol()
		}
		u.sent = append(u.sent, "V:"+typ+":"+id+":"+sym)
	case "U2":
		v, _ := m.Body.GetString(5006)
		u.sent = append(u.sent, "U2:"+v)
	}
	return nil
}

func (u *upstream) take() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.sent
	u.sent = nil
	return s
}

func newHub(t *testing.T, history, buffer int) (*Hub, *upstream) {
	u := &upstream{}
	h := NewHub(u.send, history, buffer, slog.New(slog.DiscardHandler))
	h.OnLogon()
	u.take()
	return h, u
}

func frames(c *Client) []string {
	var out []string
	for {
		select {
		case f := <-c.out:
			out = append(out, f)
		default:
			return out
		}
	}
}

func snapshot(mdReqID, sym string) *quickfix.Message {
	m := marketdatasnapshotfullrefresh.New()
	m.SetMDReqID(mdReqID)
	m.SetSymbol(sym)
	return m.ToMessage()
}

func incremental(mdReqID string) *quickfix.Message {
	m := marketdataincrementalrefresh.New()
	m.SetMDReqID(mdReqID)
	return m.ToMessage()
}

func TestSharedSubscriptionAndReplay(t *testing.T) {
	h, up := newHub(t, 3, 100)
	a, b := h.NewClient(func() {}), h.NewClient(func() {})
	h.HandleFrame(a, "35=V|262=a1|263=1|55=EURUSD")
	h.HandleFrame(b, "35=V|262=b1|263=1|55=EURUSD")
	sent := up.take()
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "V:1:gw-1-EURUSD:EURUSD") {
		t.Fatalf("upstream %v, want one shared V", sent)
	}

	h.OnServerMessage(snapshot("gw-1-EURUSD", "EURUSD"))
	for range 4 {
		h.OnServerMessage(incremental("gw-1-EURUSD"))
	}
	fa, fb := frames(a), frames(b)
	if len(fa) != 5 || !strings.Contains(fa[0], "|262=a1|") || !strings.Contains(fb[4], "|262=b1|") {
		t.Fatalf("a %v\nb %v", fa, fb)
	}

	// A late client gets the ring (last 3 frames) at once.
	c := h.NewClient(func() {})
	h.HandleFrame(c, "35=V|262=c1|263=1|55=EURUSD")
	if fc := frames(c); len(fc) != 3 || !strings.Contains(fc[0], "35=X") || !strings.Contains(fc[0], "|262=c1|") {
		t.Fatalf("replay %v", fc)
	}
	if len(up.take()) != 0 {
		t.Fatal("late client caused a new upstream V")
	}

	h.HandleFrame(a, "35=V|262=a1|263=2")
	h.Unregister(c)
	if len(up.take()) != 0 {
		t.Fatal("unsubscribed upstream while b still watches")
	}
	h.HandleFrame(b, "35=V|262=b1|263=2")
	if sent := up.take(); len(sent) != 1 || sent[0] != "V:2:gw-1-EURUSD:EURUSD" {
		t.Fatalf("last unsubscribe upstream %v", sent)
	}
	h.OnServerMessage(incremental("gw-1-EURUSD"))
	if len(frames(a))+len(frames(b)) != 0 {
		t.Fatal("frames after unsubscribe")
	}
}

func TestSubscribeOfflineThenLogon(t *testing.T) {
	up := &upstream{}
	h := NewHub(up.send, 10, 10, slog.New(slog.DiscardHandler))
	a := h.NewClient(func() {})
	h.HandleFrame(a, "35=V|262=a1|263=1|55=AAPL")
	h.HandleFrame(a, "35=U2|5005=log|5006=on")
	if len(up.take()) != 0 {
		t.Fatal("sent while offline")
	}
	h.OnLogon()
	if got := strings.Join(up.take(), ","); got != "V:1:gw-1-AAPL:AAPL,U2:on" {
		t.Fatalf("after logon %s", got)
	}
}

func TestUnknownSymbolY(t *testing.T) {
	h, _ := newHub(t, 10, 10)
	a := h.NewClient(func() {})
	h.HandleFrame(a, "35=V|262=a1|263=1|55=NOPE")
	y := marketdatarequestreject.New(field.NewMDReqID("gw-1-NOPE"))
	y.SetMDReqRejReason(enum.MDReqRejReason_UNKNOWNSYM)
	h.OnServerMessage(y.ToMessage())
	if f := frames(a); len(f) != 1 || !strings.Contains(f[0], "35=Y") || !strings.Contains(f[0], "|262=a1|") {
		t.Fatalf("Y frames %v", f)
	}
	// The failed subscription is gone, so subscribing again retries upstream.
	h.HandleFrame(a, "35=V|262=a2|263=1|55=NOPE")
}

func TestLogAndDropCopies(t *testing.T) {
	h, up := newHub(t, 10, 10)
	a, b := h.NewClient(func() {}), h.NewClient(func() {})
	h.HandleFrame(a, "35=U2|5005=log|5006=on")
	h.HandleFrame(b, "35=U2|5005=log|5006=on")
	h.HandleFrame(a, "35=U2|5005=log|5006=off")
	if got := strings.Join(up.take(), ","); got != "U2:on" {
		t.Fatalf("upstream %s", got)
	}

	u1 := mirrorlog.New(field.NewMirrorSessionID("FIX.4.4:MOCKSRV->CLIENT1"), field.NewMirrorDirection(enum.MirrorDirection_INBOUND),
		field.NewMirrorRawMsg("8=FIX.4.4|35=D"), field.NewMirrorTime(t0()))
	h.OnServerMessage(u1.ToMessage())
	er := executionreport.New(field.NewOrderID("O1"), field.NewExecID("E1"), field.NewExecType(enum.ExecType_NEW),
		field.NewOrdStatus(enum.OrdStatus_NEW), field.NewSide(enum.Side_BUY), field.NewLeavesQty(decimal.NewFromInt(1), 0),
		field.NewCumQty(decimal.Zero, 0), field.NewAvgPx(decimal.Zero, 0))
	h.OnServerMessage(er.ToMessage())
	fa, fb := frames(a), frames(b)
	if len(fa) != 1 || !strings.Contains(fa[0], "35=8") || len(fb) != 2 || !strings.Contains(fb[0], "35=U1") {
		t.Fatalf("a %v\nb %v", fa, fb)
	}

	h.Unregister(b)
	if got := strings.Join(up.take(), ","); got != "U2:off" {
		t.Fatalf("after last log client left: %s", got)
	}
}

func TestBrowserCannotSendOrders(t *testing.T) {
	h, up := newHub(t, 10, 10)
	a := h.NewClient(func() {})
	h.HandleFrame(a, "35=D|11=X|55=AAPL|54=1|38=100")
	h.HandleFrame(a, "35=V|262=a|263=0|55=AAPL")
	f := frames(a)
	if len(f) != 2 || !strings.HasPrefix(f[0], "35=j|372=D|380=3") || !strings.Contains(f[1], "372=V") {
		t.Fatalf("frames %v", f)
	}
	if len(up.take()) != 0 {
		t.Fatal("forwarded upstream")
	}
}

func TestSlowClientIsKicked(t *testing.T) {
	h, _ := newHub(t, 10, 2)
	kicked := make(chan struct{}, 2)
	a := h.NewClient(func() { kicked <- struct{}{} })
	b := h.NewClient(func() {})
	h.HandleFrame(a, "35=V|262=a|263=1|55=X")
	h.HandleFrame(b, "35=V|262=b|263=1|55=X")
	for range 5 {
		h.OnServerMessage(incremental("gw-1-X"))
		frames(b) // b keeps up
	}
	<-kicked
	select {
	case <-kicked:
		t.Fatal("kicked twice")
	default:
	}
	if len(a.out) != 2 {
		t.Fatalf("a buffered %d", len(a.out))
	}
}

func t0() time.Time { return time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC) }
