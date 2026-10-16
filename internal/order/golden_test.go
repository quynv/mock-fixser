package order

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/orderstatusrequest"
	"github.com/quynv/mock-fixser/internal/config"
)

var update = flag.Bool("update", false, "rewrite golden files")

// TestGoldenOrderFlow runs a fixed script on a fake clock with fixed seeds
// and compares every message sent, per session, with testdata (section 13).
// Run with -update after an intended change and review the diff.
func TestGoldenOrderFlow(t *testing.T) {
	h := newHarness(t, `order_rules:
  - name: reject-large-qty
    priority: 1
    match: { qty: { gt: 1000000 } }
    actions: [ { type: reject, reason: 3, text: "Order exceeds limit" } ]
  - name: timeout-test
    priority: 5
    match: { cl_ord_id: { regex: "^TIMEOUT-" } }
    actions: [ { type: ignore } ]
  - name: partial-then-fill-aapl
    priority: 10
    match: { symbol: AAPL, side: BUY, ord_type: MARKET }
    actions:
      - { type: ack, delay_ms: 50 }
      - { type: partial_fill, ratio: 0.4, delay_ms: 300 }
      - { type: fill, delay_ms: 800 }
  - name: slow-cancel
    priority: 20
    message_type: F
    actions: [ { type: ack, delay_ms: 100 } ]
default_action:
  - { type: ack, delay_ms: 20 }
  - { type: fill, delay_ms: 100 }
`, aaplQuotes(&config.Slippage{Type: "random", MinTicks: 0, MaxTicks: 3}), true)

	h.handle(nos("BUY-1", enum.Side_BUY, "1000", enum.OrdType_MARKET))  // partial then fill
	h.handle(nos("SELL-1", enum.Side_SELL, "250", enum.OrdType_MARKET)) // default chain
	h.handle(nos("BIG-1", enum.Side_SELL, "2000000", enum.OrdType_MARKET))
	h.handle(nos("TIMEOUT-1", enum.Side_SELL, "10", enum.OrdType_MARKET))
	h.handle(nos("LMT-1", enum.Side_BUY, "10", enum.OrdType_LIMIT))
	h.handle(nos("BUY-2", enum.Side_BUY, "500", enum.OrdType_MARKET))
	h.advance(400 * time.Millisecond)
	h.handle(rpl("BUY-2", "BUY-2R", "800", enum.OrdType_MARKET))
	h.handle(cxl("TIMEOUT-1", "TIMEOUT-1C"))
	h.advance(100 * time.Millisecond)
	st := orderstatusrequest.New(field.NewClOrdID("BUY-1"), field.NewSide(enum.Side_BUY))
	st.SetSymbol("AAPL")
	h.handle(st)
	h.advance(2 * time.Second)

	got := map[string][]string{}
	for {
		select {
		case o := <-h.out:
			got[o.session.TargetCompID] = append(got[o.session.TargetCompID], render(o.m))
			continue
		case <-time.After(200 * time.Millisecond):
		}
		break
	}
	var b strings.Builder
	for _, s := range []string{"CLIENT1", "UI_MONITOR"} {
		b.WriteString("# " + s + "\n")
		for _, line := range got[s] {
			b.WriteString(line + "\n")
		}
	}

	path := filepath.Join("testdata", "order_flow.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if b.String() != string(want) {
		t.Fatalf("order flow differs from %s (run with -update and review the diff)\n--- got ---\n%s", path, b.String())
	}
}

// render prints a message with | as separator and without BodyLength and
// CheckSum, so the golden file stays readable.
func render(m *quickfix.Message) string {
	var parts []string
	for _, f := range strings.Split(strings.TrimSuffix(m.String(), "\x01"), "\x01") {
		if strings.HasPrefix(f, "9=") || strings.HasPrefix(f, "10=") {
			continue
		}
		parts = append(parts, f)
	}
	return strings.Join(parts, "|")
}
