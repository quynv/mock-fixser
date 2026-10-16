package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"

	"github.com/quynv/mock-fixser/fix/tag"
)

// A client that stops reading must only cost itself conflated ticks: the
// other sessions keep getting prices and order reports (sections 2.2, 6.4).
func TestSlowClientIsConflatedOthersUnaffected(t *testing.T) {
	port := freePort(t)
	path := filepath.Join(t.TempDir(), "mock.yaml")
	var syms strings.Builder
	syms.WriteString("symbols:\n")
	for i := range 50 {
		fmt.Fprintf(&syms, "  - { symbol: S%02d, initial_price: 100, min: 50, max: 150, tick_size: 0.01, spread: 0.02, speed: { interval_ms: 2 } }\n", i)
	}
	y := fmt.Sprintf(`server:
  fix:
    listen: 127.0.0.1:%d
    data_dictionary: %s
    sessions:
      - { sender_comp_id: MOCKSRV, target_comp_id: CLIENT1 }
      - { sender_comp_id: MOCKSRV, target_comp_id: SLOW }
  monitor: { enabled: false }
%sdefault_action: [ { type: ack } ]
`, port, repoPath("spec/FIX44.xml"), syms.String())
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
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// SLOW: logs on, subscribes to every symbol, never reads again.
	var conn net.Conn
	for start := time.Now(); ; {
		conn, err = net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			break
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer conn.Close()
	logon := quickfix.NewMessage()
	for _, h := range [][2]any{{tag.BeginString, "FIX.4.4"}, {tag.MsgType, "A"}, {tag.SenderCompID, "SLOW"}, {tag.TargetCompID, "MOCKSRV"}} {
		logon.Header.SetField(h[0].(quickfix.Tag), quickfix.FIXString(h[1].(string)))
	}
	logon.Header.SetField(tag.MsgSeqNum, quickfix.FIXInt(1))
	logon.Header.SetField(tag.SendingTime, quickfix.FIXUTCTimestamp{Time: time.Now()})
	logon.Body.SetField(tag.EncryptMethod, quickfix.FIXString("0"))
	logon.Body.SetField(tag.HeartBtInt, quickfix.FIXInt(30))
	conn.Write([]byte(logon.String()))
	time.Sleep(300 * time.Millisecond)

	v := "8=FIX.4.4\x019=0\x0135=V\x0134=2\x0149=SLOW\x0152=" + time.Now().UTC().Format("20060102-15:04:05.000") +
		"\x0156=MOCKSRV\x01262=slow\x01263=1\x01264=1\x01267=2\x01269=0\x01269=1\x01146=50\x01"
	for i := range 50 {
		v += fmt.Sprintf("55=S%02d\x01", i)
	}
	conn.Write(fixFrame(v))

	// The normal client keeps working while SLOW's socket buffers fill up.
	c := dial(t, port)
	c.subscribe(t, "fast", "S00", "S01")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.waitFor(t, "X", "fast")
	}
	id := quickfix.SessionID{BeginString: "FIX.4.4", SenderCompID: "MOCKSRV", TargetCompID: "SLOW"}
	if n := a.pub.Conflated(id); n == 0 {
		t.Fatal("no ticks conflated for the slow client")
	}
}

// fixFrame fixes BodyLength and CheckSum of a raw message ending in SOH.
func fixFrame(raw string) []byte {
	i := strings.Index(raw, "\x0135=")
	body := raw[i+1:]
	head := fmt.Sprintf("8=FIX.4.4\x019=%d\x01", len(body))
	msg := head + body
	sum := 0
	for _, b := range []byte(msg) {
		sum += int(b)
	}
	return []byte(fmt.Sprintf("%s10=%03d\x01", msg, sum%256))
}
