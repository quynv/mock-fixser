package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"
	qconfig "github.com/quickfixgo/quickfix/config"
	"github.com/shopspring/decimal"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/marketdatarequest"
	"github.com/quynv/mock-fixser/fix/fix44/marketdatasnapshotfullrefresh"
	"github.com/quynv/mock-fixser/fix/fix44/newordersingle"
	"github.com/quynv/mock-fixser/fix/tag"
	"github.com/quynv/mock-fixser/internal/tlsgen"
)

// lpClient behaves like a typical LP client of mockfix:
// TLS with a client certificate and no server check, reset
// on logon and logout, ResetSeqNumFlag and Password on Logon, and no data
// dictionary.
type lpClient struct {
	*client
}

func (c *lpClient) ToAdmin(m *quickfix.Message, _ quickfix.SessionID) {
	if mt, _ := m.MsgType(); mt == "A" {
		m.Body.SetField(tag.EncryptMethod, quickfix.FIXString("0"))
		m.Body.SetField(tag.ResetSeqNumFlag, quickfix.FIXBoolean(true))
		m.Body.SetField(tag.Password, quickfix.FIXString("secret"))
	}
}

func dialLPClient(t *testing.T, port int, certs, sender, target string) *lpClient {
	t.Helper()
	settings := quickfix.NewSettings()
	g := settings.GlobalSettings()
	for k, v := range map[string]string{
		qconfig.SocketConnectHost: "127.0.0.1", qconfig.SocketConnectPort: strconv.Itoa(port),
		qconfig.HeartBtInt: "30", qconfig.SenderCompID: sender, qconfig.TargetCompID: target,
		qconfig.SocketPrivateKeyFile:  filepath.Join(certs, "client.key"),
		qconfig.SocketCertificateFile: filepath.Join(certs, "client.crt"),
		qconfig.SocketCAFile:          filepath.Join(certs, "ca.crt"),
		qconfig.SocketUseSSL:          "Y", qconfig.SocketInsecureSkipVerify: "Y",
		qconfig.ResetOnLogon: "Y", qconfig.ResetOnLogout: "Y", qconfig.ReconnectInterval: "1",
	} {
		g.Set(k, v)
	}
	ss := quickfix.NewSessionSettings()
	ss.Set(qconfig.BeginString, "FIX.4.4")
	id, err := settings.AddSession(ss)
	if err != nil {
		t.Fatal(err)
	}
	c := &lpClient{client: &client{id: id, logon: make(chan struct{}, 4), msgs: make(chan *quickfix.Message, 10000)}}
	ini, err := quickfix.NewInitiator(c, quickfix.NewMemoryStoreFactory(), settings, quickfix.NewNullLogFactory())
	if err != nil {
		t.Fatal(err)
	}
	if err := ini.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ini.Stop)
	select {
	case <-c.logon:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not log on", sender)
	}
	return c
}

// subscribe asks for bids only, full refresh, full depth.
func (c *lpClient) subscribe(t *testing.T, mdReqID, symbol string) {
	t.Helper()
	m := marketdatarequest.New(field.NewMDReqID(mdReqID),
		field.NewSubscriptionRequestType(enum.SubscriptionRequestType_SNAPSHOTUPDATE), field.NewMarketDepth(0))
	m.SetMDUpdateType(enum.MDUpdateType_FULL)
	types := marketdatarequest.NewNoMDEntryTypesRepeatingGroup()
	types.Add().SetMDEntryType(enum.MDEntryType_BID)
	m.SetNoMDEntryTypes(types)
	syms := marketdatarequest.NewNoRelatedSymRepeatingGroup()
	syms.Add().SetSymbol(symbol)
	m.SetNoRelatedSym(syms)
	if err := quickfix.SendToTarget(m, c.id); err != nil {
		t.Fatal(err)
	}
}

// acceptable applies the client's checks to a W: at least two
// entries, a bid and an offer, and QuoteCondition A on every entry.
func acceptable(t *testing.T, m *quickfix.Message) (tradable bool, reason string) {
	t.Helper()
	g, err := marketdatasnapshotfullrefresh.FromMessage(m).GetNoMDEntries()
	if err != nil {
		return false, err.Error()
	}
	if g.Len() < 2 {
		return false, "MDEntries data size is less than 2"
	}
	bid, offer, tradable := false, false, true
	for i := range g.Len() {
		e := g.Get(i)
		typ, _ := e.GetMDEntryType()
		bid = bid || typ == enum.MDEntryType_BID
		offer = offer || typ == enum.MDEntryType_OFFER
		if qc, err := e.GetQuoteCondition(); err != nil || qc != "A" {
			tradable = false
		}
	}
	if !bid || !offer {
		return false, "Buy or sell price is missing"
	}
	return tradable, ""
}

func TestLPClientCompatibility(t *testing.T) {
	certs := t.TempDir()
	if err := tlsgen.Generate(certs, []string{"127.0.0.1"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	path := filepath.Join(t.TempDir(), "mock.yaml")
	y := fmt.Sprintf(`server:
  fix:
    listen: 127.0.0.1:%d
    data_dictionary: %s
    tls: { cert_file: %s, key_file: %s, client_auth: request }
    sessions:
      - { sender_comp_id: MOCKSRV_T, target_comp_id: CLIENT_T }
      - sender_comp_id: MOCKSRV_Q
        target_comp_id: CLIENT_Q
        market_data: { send_both_sides: true }
  monitor: { enabled: false }
symbols:
  - { symbol: XAUUSD, initial_price: 2400, min: 2000, max: 2800, tick_size: 0.01, spread: 0.30, quote_condition: A,
      seed: 1, depth_levels: 2, speed: { interval_ms: 50 } }
default_action: [ { type: ack }, { type: fill } ]
`, port, repoPath("spec/FIX44.xml"), filepath.Join(certs, "server.crt"), filepath.Join(certs, "server.key"))
	if err := os.WriteFile(path, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := New(path, slog.New(slog.DiscardHandler), Options{NoUI: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	// price session
	q := dialLPClient(t, port, certs, "CLIENT_Q", "MOCKSRV_Q")
	q.subscribe(t, "md-1", "XAUUSD")
	for range 3 { // the snapshot and two ticks, all W (full refresh)
		w := q.waitFor(t, "W", "md-1")
		if ok, why := acceptable(t, w); !ok {
			t.Fatalf("client would drop or mark non-tradable: %q in %s", why, w)
		}
	}

	// QuoteCancel for all quotes: the gateway resubscribes, prices resume.
	res := a.pub.QuoteCancel(nil, nil)
	if len(res.Sessions) != 1 {
		t.Fatalf("QuoteCancel %+v", res)
	}
	z := q.waitFor(t, "Z", "")
	if typ, _ := z.Body.GetString(tag.QuoteCancelType); typ != "4" {
		t.Fatalf("QuoteCancelType %s", typ)
	}
	q.subscribe(t, "md-2", "XAUUSD")
	q.waitFor(t, "W", "md-2")

	// order session: Market FOK with HandlInst 1 and a 2-decimal quantity.
	o := dialLPClient(t, port, certs, "CLIENT_T", "MOCKSRV_T")
	d := newordersingle.New(field.NewClOrdID("0b6d8a52-2f9e-7d3c-9b1a-1c2d3e4f5a6b"), field.NewSide(enum.Side_BUY),
		field.NewTransactTime(time.Now()), field.NewOrdType(enum.OrdType_MARKET))
	d.SetHandlInst("1")
	d.SetSymbol("XAUUSD")
	d.SetOrderQty(decimal.RequireFromString("1.50"), 2)
	d.SetTimeInForce(enum.TimeInForce_FILLORKILL)
	start := time.Now()
	if err := quickfix.SendToTarget(d, o.id); err != nil {
		t.Fatal(err)
	}
	for {
		er := o.waitFor(t, "8", "")
		status, _ := er.Body.GetString(tag.OrdStatus)
		if status != "2" && status != "8" && status != "4" && status != "C" {
			continue // the client ignores intermediate reports
		}
		lastPx, err := er.Body.GetString(tag.LastPx)
		if status != "2" || err != nil || lastPx == "" {
			t.Fatalf("final report OrdStatus %s LastPx %q: %s", status, lastPx, er)
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("slower than the client's 5 s timeout")
		}
		break
	}
}
