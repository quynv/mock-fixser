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

	"github.com/quynv/mock-fixser/fix/tag"
)

// versionClient is an initiator of any FIX version. It validates incoming
// messages against its own dictionary, so a field the server sends that
// does not exist in that version fails the test.
type versionClient struct {
	*client
	rejects chan *quickfix.Message
}

func (c *versionClient) FromAdmin(m *quickfix.Message, _ quickfix.SessionID) quickfix.MessageRejectError {
	return nil
}

func dialVersion(t *testing.T, port int, sender, version string) *versionClient {
	t.Helper()
	settings := quickfix.NewSettings()
	ss := quickfix.NewSessionSettings()
	ss.Set(qconfig.SenderCompID, sender)
	ss.Set(qconfig.TargetCompID, "MOCKSRV")
	ss.Set(qconfig.HeartBtInt, "30")
	ss.Set(qconfig.SocketConnectHost, "127.0.0.1")
	ss.Set(qconfig.SocketConnectPort, strconv.Itoa(port))
	ss.Set(qconfig.ReconnectInterval, "1")
	switch version {
	case "FIX.4.2":
		ss.Set(qconfig.BeginString, "FIX.4.2")
		ss.Set(qconfig.DataDictionary, repoPath("spec/FIX42.xml"))
	case "FIX.5.0SP2":
		ss.Set(qconfig.BeginString, "FIXT.1.1")
		ss.Set(qconfig.DefaultApplVerID, "FIX.5.0SP2")
		ss.Set(qconfig.TransportDataDictionary, repoPath("spec/FIXT11.xml"))
		ss.Set(qconfig.AppDataDictionary, repoPath("spec/FIX50SP2.xml"))
	}
	id, err := settings.AddSession(ss)
	if err != nil {
		t.Fatal(err)
	}
	c := &versionClient{client: &client{id: id, logon: make(chan struct{}, 1), msgs: make(chan *quickfix.Message, 10000)}}
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
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not log on", version)
	}
	return c
}

// send builds a message from tag/value pairs (groups as written).
func (c *versionClient) send(t *testing.T, msgType string, fields ...any) {
	t.Helper()
	m := quickfix.NewMessage()
	m.Header.SetField(tag.MsgType, quickfix.FIXString(msgType))
	for i := 0; i < len(fields); i += 2 {
		switch v := fields[i+1].(type) {
		case string:
			m.Body.SetField(quickfix.Tag(fields[i].(int)), quickfix.FIXString(v))
		case *quickfix.RepeatingGroup:
			m.Body.SetGroup(v)
		}
	}
	if err := quickfix.SendToTarget(m, c.id); err != nil {
		t.Fatal(err)
	}
}

func group(countTag int, delim int, values ...string) *quickfix.RepeatingGroup {
	g := quickfix.NewRepeatingGroup(quickfix.Tag(countTag), quickfix.GroupTemplate{quickfix.GroupElement(quickfix.Tag(delim))})
	for _, v := range values {
		g.Add().SetField(quickfix.Tag(delim), quickfix.FIXString(v))
	}
	return g
}

func get(m *quickfix.Message, t quickfix.Tag) string {
	v, _ := m.Body.GetString(t)
	return v
}

func TestFIX42AndFIX50Sessions(t *testing.T) {
	port := freePort(t)
	path := filepath.Join(t.TempDir(), "mock.yaml")
	y := fmt.Sprintf(`server:
  fix:
    listen: 127.0.0.1:%d
    data_dictionary: %s
    sessions:
      - { sender_comp_id: MOCKSRV, target_comp_id: CLIENT44 }
      - { sender_comp_id: MOCKSRV, target_comp_id: CLIENT42, version: FIX.4.2 }
      - { sender_comp_id: MOCKSRV, target_comp_id: CLIENT50, version: FIX.5.0SP2 }
  monitor: { enabled: false }
%sorder_rules:
  - { name: rest, priority: 1, match: { cl_ord_id: { regex: "^REST" } }, actions: [ { type: ack } ] }
default_action: [ { type: ack }, { type: fill } ]
`, port, repoPath("spec/FIX44.xml"), symbolsYAML)
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

	for _, tc := range []struct{ sender, version string }{{"CLIENT42", "FIX.4.2"}, {"CLIENT50", "FIX.5.0SP2"}} {
		t.Run(tc.version, func(t *testing.T) {
			c := dialVersion(t, port, tc.sender, tc.version)
			now := time.Now().UTC().Format("20060102-15:04:05.000")

			// Market data.
			c.send(t, "V", 262, "md", 263, "1", 264, "1", 267, group(267, 269, "0", "1"), 146, group(146, 55, "EURUSD"))
			c.waitFor(t, "W", "md")
			c.waitFor(t, "X", "md")
			c.send(t, "V", 262, "bad", 263, "1", 264, "1", 267, group(267, 269, "0", "1"), 146, group(146, 55, "NOPE"))
			c.waitFor(t, "Y", "bad")

			// An order that fills.
			order := []any{11, "F1", 21, "1", 55, "AAPL", 54, "1", 60, now, 38, "100", 40, "1"}
			c.send(t, "D", order...)
			ack, fill := c.waitFor(t, "8", ""), c.waitFor(t, "8", "")
			if tc.version == "FIX.4.2" {
				if get(ack, 20) != "0" || get(ack, 150) != "0" || get(fill, 150) != "2" || get(fill, 39) != "2" {
					t.Fatalf("FIX 4.2 reports: ack 20=%s 150=%s, fill 150=%s 39=%s", get(ack, 20), get(ack, 150), get(fill, 150), get(fill, 39))
				}
			} else if get(fill, 150) != "F" {
				t.Fatalf("fill ExecType %s, want F", get(fill, 150))
			}

			// Status, cancel and cancel reject.
			c.send(t, "D", 11, "REST1", 21, "1", 55, "AAPL", 54, "2", 60, now, 38, "10", 40, "1")
			c.waitFor(t, "8", "")
			c.send(t, "H", 11, "REST1", 55, "AAPL", 54, "2")
			st := c.waitFor(t, "8", "")
			if tc.version == "FIX.4.2" && (get(st, 20) != "3" || get(st, 150) != "0") {
				t.Fatalf("FIX 4.2 status: 20=%s 150=%s", get(st, 20), get(st, 150))
			} else if tc.version != "FIX.4.2" && get(st, 150) != "I" {
				t.Fatalf("status ExecType %s", get(st, 150))
			}
			c.send(t, "F", 41, "REST1", 11, "REST2", 55, "AAPL", 54, "2", 60, now, 38, "10")
			if cx := c.waitFor(t, "8", ""); get(cx, 150) != "4" {
				t.Fatalf("cancel ExecType %s", get(cx, 150))
			}
			c.send(t, "F", 41, "NOPE", 11, "X1", 55, "AAPL", 54, "2", 60, now, 38, "10")
			if rj := c.waitFor(t, "9", ""); get(rj, 102) != "1" {
				t.Fatalf("CxlRejReason %s", get(rj, 102))
			}
		})
	}

	// The sessions list shows each version.
	sessions := a.srv.Sessions(a.Config())
	want := map[string]string{"CLIENT44": "FIX.4.4", "CLIENT42": "FIX.4.2", "CLIENT50": "FIX.5.0SP2"}
	for _, s := range sessions {
		if want[s.TargetCompID] != s.Version {
			t.Errorf("%s version %s, want %s", s.TargetCompID, s.Version, want[s.TargetCompID])
		}
	}
}
