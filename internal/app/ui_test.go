package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/quickfixgo/quickfix"
	"github.com/shopspring/decimal"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/newordersingle"
	"github.com/quynv/mock-fixser/internal/fixserver"
)

// startUI runs the full app (HTTP, gateway, monitor) and returns the HTTP
// base URL.
func startUI(t *testing.T) (base string, fixPort int) {
	t.Helper()
	fixPort, monPort := freePort(t), freePort(t)
	path := filepath.Join(t.TempDir(), "mock.yaml")
	y := fmt.Sprintf(`server:
  fix:
    listen: 127.0.0.1:%d
    data_dictionary: %s
    sessions:
      - { sender_comp_id: MOCKSRV, target_comp_id: CLIENT1 }
  monitor:
    enabled: true
    listen: 127.0.0.1:%d
    drop_copy: true
    mirror_log: true
  http:
    listen: 127.0.0.1:0
    history_ticks: 50
%sdefault_action: [ { type: ack }, { type: fill } ]
`, fixPort, repoPath("spec/FIX44.xml"), monPort, symbolsYAML)
	if err := os.WriteFile(path, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := New(path, slog.New(slog.DiscardHandler), Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for a.HTTPAddr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("HTTP server did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "http://" + a.HTTPAddr(), fixPort
}

type ws struct {
	t      *testing.T
	conn   *websocket.Conn
	frames chan string
}

func dialWS(t *testing.T, base string) *ws {
	t.Helper()
	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(base, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadLimit(1 << 20)
	w := &ws{t: t, conn: conn, frames: make(chan string, 10000)}
	go func() {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				close(w.frames)
				return
			}
			w.frames <- string(data)
		}
	}()
	t.Cleanup(func() { conn.CloseNow() })
	return w
}

func (w *ws) send(frame string) {
	w.t.Helper()
	if err := w.conn.Write(w.t.Context(), websocket.MessageText, []byte(frame)); err != nil {
		w.t.Fatal(err)
	}
}

// waitFor returns the first frame containing every part.
func (w *ws) waitFor(parts ...string) string {
	w.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case f, ok := <-w.frames:
			if !ok {
				w.t.Fatal("websocket closed")
			}
			match := true
			for _, p := range parts {
				if !strings.Contains(f, p) {
					match = false
					break
				}
			}
			if match {
				return f
			}
		case <-deadline:
			w.t.Fatalf("no frame with %v", parts)
		}
	}
}

func httpJSON(t *testing.T, method, url, body string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, url, err, data)
		}
	}
	return res.StatusCode
}

func TestUIOverWebSocketAndREST(t *testing.T) {
	base, fixPort := startUI(t)

	a := dialWS(t, base)
	a.send("35=V|262=chart-a|263=1|55=EURUSD")
	a.waitFor("35=W", "|262=chart-a|", "|55=EURUSD|")
	a.waitFor("35=X", "|262=chart-a|")

	// A second tab on the same symbol gets the history at once.
	b := dialWS(t, base)
	b.send("35=V|262=chart-b|263=1|55=EURUSD")
	b.waitFor("35=W", "|262=chart-b|")

	// The browser cannot send orders.
	a.send("35=D|11=X|55=EURUSD|54=1|38=1|40=1")
	a.waitFor("35=j", "372=D")

	// Runtime command: the forced price shows up in the stream.
	var st struct{ Mid float64 }
	if code := httpJSON(t, "POST", base+"/api/symbols/AAPL/pause", "", nil); code != 200 {
		t.Fatalf("pause: %d", code)
	}
	a.send("35=V|262=aapl|263=1|55=AAPL")
	a.waitFor("35=W", "|262=aapl|")
	httpJSON(t, "POST", base+"/api/symbols/AAPL/price", `{"mid":250}`, &st)
	a.waitFor("35=X", "|262=aapl|", "|270=249.99|")

	// Log mirror and drop copies.
	a.send("35=U2|5005=log|5006=on")
	c := dial(t, fixPort)
	time.Sleep(200 * time.Millisecond) // let U2 reach the server
	d := newordersingle.New(field.NewClOrdID("UI-1"), field.NewSide(enum.Side_BUY), field.NewTransactTime(time.Now()),
		field.NewOrdType(enum.OrdType_MARKET))
	d.SetSymbol("EURUSD")
	d.SetOrderQty(decimal.NewFromInt(1000), 0)
	if err := quickfix.SendToTarget(d, c.id); err != nil {
		t.Fatal(err)
	}
	a.waitFor("35=U1", "5002=I", "5003=", "35=D", "11=UI-1")
	a.waitFor("35=8", "11=UI-1", "150=F", "5001=FIX.4.4:MOCKSRV->CLIENT1")
	b.waitFor("35=8", "11=UI-1", "150=F") // drop copies go to every tab

	var sessions []fixserver.SessionInfo
	httpJSON(t, "GET", base+"/api/sessions", "", &sessions)
	if len(sessions) != 2 || !sessions[0].LoggedOn || !sessions[1].LoggedOn || sessions[0].NextTargetSeq < 2 {
		t.Fatalf("sessions %+v", sessions)
	}
	var orders []map[string]any
	httpJSON(t, "GET", base+"/api/orders?session=CLIENT1", "", &orders)
	if len(orders) != 1 || orders[0]["status"] != "2" {
		t.Fatalf("orders %+v", orders)
	}

	res, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	for _, m := range []string{"mockfix_ticks_total{symbol=\"EURUSD\"}", "mockfix_orders_total{", "mockfix_sessions_logged_on 2",
		"mockfix_fix_messages_total{direction=\"in\",msg_type=\"D\"", "mockfix_md_subscriptions{symbol=\"EURUSD\"} 1"} {
		if !strings.Contains(string(body), m) {
			t.Errorf("metrics missing %s", m)
		}
	}

	if code := httpJSON(t, "POST", base+"/api/sessions/CLIENT1/logout", "", nil); code != 200 {
		t.Fatalf("logout: %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		httpJSON(t, "GET", base+"/api/sessions", "", &sessions)
		if !sessions[0].LoggedOn {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CLIENT1 still logged on after logout")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
