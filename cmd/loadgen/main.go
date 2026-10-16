// Command loadgen load-tests mockfix (design doc, section 13): many client
// sessions subscribe to every symbol and send orders at a fixed rate; it
// reports order latency, market data throughput and the server's CPU,
// memory, tick rate and conflation from /metrics.
//
// Generate a matching config, start mockfix with it, then run the load:
//
//	go run ./cmd/loadgen -gen-config /tmp/load.yaml -sessions 10 -symbols 200 -tps 25
//	bin/mockfix --config /tmp/load.yaml --log-level warn &
//	go run ./cmd/loadgen -sessions 10 -symbols 200 -orders 100 -duration 30s
package main

import (
	"bufio"
	"flag"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quickfixgo/quickfix"
	"github.com/quickfixgo/quickfix/config"
	"github.com/shopspring/decimal"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/marketdatarequest"
	"github.com/quynv/mock-fixser/fix/fix44/newordersingle"
)

func main() {
	genConfig := flag.String("gen-config", "", "write a load-test config to this file and exit")
	addr := flag.String("addr", "127.0.0.1:9876", "mockfix FIX address")
	metricsURL := flag.String("metrics", "http://127.0.0.1:8080/metrics", "mockfix /metrics URL (empty to skip)")
	sessions := flag.Int("sessions", 10, "client sessions (LOAD01..)")
	symbols := flag.Int("symbols", 200, "symbols (SYM0001..), subscribed by every session")
	tps := flag.Float64("tps", 25, "ticks per second per symbol (with -gen-config)")
	orders := flag.Float64("orders", 100, "orders per second, over all sessions")
	duration := flag.Duration("duration", 30*time.Second, "test duration")
	flag.Parse()

	if *genConfig != "" {
		if err := writeConfig(*genConfig, *sessions, *symbols, *tps, *addr); err != nil {
			fail(err)
		}
		fmt.Printf("wrote %s: %d sessions, %d symbols at %g ticks/s (%g ticks/s in total)\n",
			*genConfig, *sessions, *symbols, *tps, float64(*symbols)**tps)
		return
	}
	if err := run(*addr, *metricsURL, *sessions, *symbols, *orders, *duration); err != nil {
		fail(err)
	}
}

func sessionName(i int) string { return fmt.Sprintf("LOAD%02d", i+1) }
func symbolName(i int) string  { return fmt.Sprintf("SYM%04d", i+1) }

func writeConfig(path string, sessions, symbols int, tps float64, addr string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Load test config written by cmd/loadgen.\nserver:\n  fix:\n    listen: %s\n    sessions:\n", addr)
	for i := range sessions {
		fmt.Fprintf(&b, "      - { sender_comp_id: MOCKSRV, target_comp_id: %s }\n", sessionName(i))
	}
	b.WriteString("  monitor: { enabled: true, mirror_log: false }\nsymbols:\n")
	for i := range symbols {
		fmt.Fprintf(&b, "  - { symbol: %s, initial_price: 100, min: 50, max: 150, tick_size: 0.01, spread: 0.02, "+
			"model: random_walk, volatility: 0.05, seed: %d, depth_levels: 1, speed: { ticks_per_second: %g } }\n", symbolName(i), i+1, tps)
	}
	// No delays: the latency measured is the processing time (NFR p99 < 5 ms).
	b.WriteString("default_action: [ { type: ack }, { type: fill } ]\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

type stats struct {
	logons    atomic.Int64
	mdMsgs    atomic.Int64
	mdEntries atomic.Int64
	reports   atomic.Int64
	rejects   atomic.Int64
	sent      sync.Map // ClOrdID -> time.Time
	mu        sync.Mutex
	latencies []time.Duration
}

type app struct {
	st *stats
}

func (a *app) OnCreate(quickfix.SessionID)                   {}
func (a *app) OnLogon(quickfix.SessionID)                    { a.st.logons.Add(1) }
func (a *app) OnLogout(quickfix.SessionID)                   {}
func (a *app) ToAdmin(*quickfix.Message, quickfix.SessionID) {}
func (a *app) ToApp(*quickfix.Message, quickfix.SessionID) error {
	return nil
}
func (a *app) FromAdmin(*quickfix.Message, quickfix.SessionID) quickfix.MessageRejectError {
	return nil
}

func (a *app) FromApp(m *quickfix.Message, _ quickfix.SessionID) quickfix.MessageRejectError {
	mt, _ := m.MsgType()
	switch mt {
	case "W", "X":
		a.st.mdMsgs.Add(1)
		if n, err := m.Body.GetInt(268); err == nil {
			a.st.mdEntries.Add(int64(n))
		}
	case "8":
		a.st.reports.Add(1)
		id, _ := m.Body.GetString(11)
		if t, ok := a.st.sent.LoadAndDelete(id); ok { // first report = ack
			d := time.Since(t.(time.Time))
			a.st.mu.Lock()
			a.st.latencies = append(a.st.latencies, d)
			a.st.mu.Unlock()
		}
	case "Y", "j", "9":
		a.st.rejects.Add(1)
	}
	return nil
}

func run(addr, metricsURL string, sessions, symbols int, ordersPerSec float64, duration time.Duration) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	settings := quickfix.NewSettings()
	g := settings.GlobalSettings()
	g.Set(config.BeginString, quickfix.BeginStringFIX44)
	g.Set(config.TargetCompID, "MOCKSRV")
	g.Set(config.SocketConnectHost, host)
	g.Set(config.SocketConnectPort, port)
	g.Set(config.HeartBtInt, "30")
	g.Set(config.ReconnectInterval, "1")
	// No DataDictionary: messages are counted, not validated, so the client
	// is not the bottleneck.
	ids := make([]quickfix.SessionID, sessions)
	for i := range sessions {
		ss := quickfix.NewSessionSettings()
		ss.Set(config.SenderCompID, sessionName(i))
		if ids[i], err = settings.AddSession(ss); err != nil {
			return err
		}
	}
	st := &stats{}
	ini, err := quickfix.NewInitiator(&app{st}, quickfix.NewMemoryStoreFactory(), settings, quickfix.NewNullLogFactory())
	if err != nil {
		return err
	}
	if err := ini.Start(); err != nil {
		return err
	}
	defer ini.Stop()

	deadline := time.Now().Add(10 * time.Second)
	for st.logons.Load() < int64(sessions) {
		if time.Now().After(deadline) {
			return fmt.Errorf("only %d of %d sessions logged on", st.logons.Load(), sessions)
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Printf("%d sessions logged on; subscribing to %d symbols each\n", sessions, symbols)

	// One V per session with every symbol (NoRelatedSym > 1).
	for i, id := range ids {
		v := marketdatarequest.New(field.NewMDReqID(fmt.Sprintf("md-%d", i)),
			field.NewSubscriptionRequestType(enum.SubscriptionRequestType_SNAPSHOTUPDATE), field.NewMarketDepth(1))
		types := marketdatarequest.NewNoMDEntryTypesRepeatingGroup()
		types.Add().SetMDEntryType(enum.MDEntryType_BID)
		types.Add().SetMDEntryType(enum.MDEntryType_OFFER)
		v.SetNoMDEntryTypes(types)
		syms := marketdatarequest.NewNoRelatedSymRepeatingGroup()
		for s := range symbols {
			syms.Add().SetSymbol(symbolName(s))
		}
		v.SetNoRelatedSym(syms)
		if err := quickfix.SendToTarget(v, id); err != nil {
			return err
		}
	}
	time.Sleep(time.Second) // initial snapshots
	before, _ := scrape(metricsURL)
	st.mdMsgs.Store(0)
	st.mdEntries.Store(0)
	start := time.Now()

	// Orders at a fixed rate, round robin over sessions and symbols.
	sentOrders := 0
	if ordersPerSec > 0 {
		tick := time.NewTicker(time.Duration(float64(time.Second) / ordersPerSec))
		for now := range tick.C {
			if now.Sub(start) >= duration {
				break
			}
			id := ids[sentOrders%sessions]
			clOrdID := "L" + strconv.Itoa(sentOrders)
			side := enum.Side_BUY
			if sentOrders%2 == 1 {
				side = enum.Side_SELL
			}
			d := newordersingle.New(field.NewClOrdID(clOrdID), field.NewSide(side), field.NewTransactTime(now), field.NewOrdType(enum.OrdType_MARKET))
			d.SetSymbol(symbolName(sentOrders % symbols))
			d.SetOrderQty(decimal.NewFromInt(100), 0)
			st.sent.Store(clOrdID, time.Now())
			if err := quickfix.SendToTarget(d, id); err != nil {
				return err
			}
			sentOrders++
		}
		tick.Stop()
	} else {
		time.Sleep(duration)
	}
	elapsed := time.Since(start)
	time.Sleep(500 * time.Millisecond) // last acks
	after, _ := scrape(metricsURL)

	report(st, sentOrders, elapsed, before, after)
	return nil
}

func report(st *stats, sentOrders int, elapsed time.Duration, before, after map[string]float64) {
	secs := elapsed.Seconds()
	fmt.Printf("\nduration          %.1fs\n", secs)
	fmt.Printf("market data       %.0f msgs/s, %.0f entries/s received (all sessions)\n",
		float64(st.mdMsgs.Load())/secs, float64(st.mdEntries.Load())/secs)

	st.mu.Lock()
	lat := slices.Clone(st.latencies)
	st.mu.Unlock()
	slices.Sort(lat)
	pct := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return lat[int(math.Min(float64(len(lat)-1), math.Ceil(p*float64(len(lat)))-1))]
	}
	fmt.Printf("orders            %d sent, %d acknowledged, %d rejects\n", sentOrders, len(lat), st.rejects.Load())
	if len(lat) > 0 {
		fmt.Printf("order latency     p50 %v  p90 %v  p99 %v  max %v  (D -> first ExecutionReport, incl. network)\n",
			pct(0.50).Round(time.Microsecond), pct(0.90).Round(time.Microsecond), pct(0.99).Round(time.Microsecond),
			lat[len(lat)-1].Round(time.Microsecond))
	}
	if before == nil || after == nil {
		return
	}
	d := func(name string) float64 { return after[name] - before[name] }
	if p99, ok := histQuantile(before, after, `mockfix_order_latency_seconds_bucket{msg_type="D"`, 0.99); ok {
		fmt.Printf("server handling   p99 %v for D (mockfix_order_latency_seconds)\n", time.Duration(p99*float64(time.Second)).Round(time.Microsecond))
	}
	fmt.Printf("server ticks      %.0f ticks/s generated\n", d("mockfix_ticks_total")/secs)
	fmt.Printf("server conflated  %.0f ticks dropped for slow sessions\n", d("mockfix_md_conflated_total"))
	fmt.Printf("server CPU        %.0f%% of one core\n", 100*d("process_cpu_seconds_total")/secs)
	if rss := after["process_resident_memory_bytes"]; rss > 0 {
		fmt.Printf("server memory     %.0f MiB resident\n", rss/(1<<20))
	} else { // no process collector on macOS
		fmt.Printf("server memory     %.0f MiB from the Go runtime, %.0f MiB heap in use\n",
			after["go_memstats_sys_bytes"]/(1<<20), after["go_memstats_heap_inuse_bytes"]/(1<<20))
	}
}

// histQuantile estimates a quantile from the bucket deltas of one
// histogram series (prefix up to, but not including, its le label).
func histQuantile(before, after map[string]float64, prefix string, q float64) (float64, bool) {
	type bucket struct{ le, n float64 }
	var bs []bucket
	for k, v := range after {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		i := strings.Index(k, `le="`)
		if i < 0 {
			continue
		}
		leStr := k[i+4 : strings.IndexByte(k[i+4:], '"')+i+4]
		le, err := strconv.ParseFloat(leStr, 64)
		if err != nil {
			le = math.Inf(1)
		}
		bs = append(bs, bucket{le, v - before[k]})
	}
	if len(bs) == 0 {
		return 0, false
	}
	slices.SortFunc(bs, func(a, b bucket) int {
		switch {
		case a.le < b.le:
			return -1
		case a.le > b.le:
			return 1
		}
		return 0
	})
	total := bs[len(bs)-1].n
	if total == 0 {
		return 0, false
	}
	for _, b := range bs {
		if b.n >= q*total {
			return b.le, true // upper bound of the bucket
		}
	}
	return bs[len(bs)-1].le, true
}

// scrape sums every sample of each metric family in /metrics; histogram
// buckets keep their labels.
func scrape(url string) (map[string]float64, error) {
	if url == "" {
		return nil, nil
	}
	res, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	out := map[string]float64{}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		key := line[:strings.LastIndexByte(line, ' ')]
		v, err := strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64)
		if err != nil {
			continue
		}
		if strings.Contains(key, "_bucket{") {
			out[key] = v
			continue
		}
		name, _, _ := strings.Cut(key, "{")
		out[name] += v
	}
	return out, sc.Err()
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "loadgen:", err)
	os.Exit(1)
}
