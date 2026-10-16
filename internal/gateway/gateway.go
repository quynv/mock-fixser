package gateway

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"

	"github.com/coder/websocket"
	"github.com/quickfixgo/quickfix"
	qconfig "github.com/quickfixgo/quickfix/config"

	"github.com/quynv/mock-fixser/internal/config"
)

// ClientBuffer is the per-WebSocket frame buffer (section 8.2).
const ClientBuffer = 1000

// Gateway is a quickfixgo initiator for UI_MONITOR -> MOCKSRV over
// loopback, plus the WebSocket hub. It uses the same FIX code path as any
// external client.
type Gateway struct {
	log *slog.Logger
	id  quickfix.SessionID
	ini *quickfix.Initiator
	hub *Hub
}

func New(cfg *config.Config, log *slog.Logger) (*Gateway, error) {
	mon := cfg.Server.Monitor
	host, port, err := net.SplitHostPort(mon.Listen)
	if err != nil {
		return nil, err
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	g := &Gateway{log: log.With("component", "gateway")}
	settings := quickfix.NewSettings()
	ss := quickfix.NewSessionSettings()
	ss.Set(qconfig.BeginString, cfg.Server.FIX.Version)
	ss.Set(qconfig.SenderCompID, mon.TargetCompID)
	ss.Set(qconfig.TargetCompID, mon.SenderCompID)
	ss.Set(qconfig.DataDictionary, cfg.Server.FIX.DataDictionary)
	ss.Set(qconfig.SocketConnectHost, host)
	ss.Set(qconfig.SocketConnectPort, port)
	ss.Set(qconfig.HeartBtInt, "30")
	ss.Set(qconfig.ReconnectInterval, "1")
	if g.id, err = settings.AddSession(ss); err != nil {
		return nil, err
	}
	g.hub = NewHub(func(m *quickfix.Message) error { return quickfix.SendToTarget(m, g.id) },
		cfg.Server.HTTP.HistoryTicks, ClientBuffer, g.log)
	if g.ini, err = quickfix.NewInitiator(app{g}, quickfix.NewMemoryStoreFactory(), settings, quickfix.NewNullLogFactory()); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *Gateway) Start() error { return g.ini.Start() }
func (g *Gateway) Stop()        { g.ini.Stop() }

// app is the initiator's quickfix.Application.
type app struct{ g *Gateway }

func (a app) OnCreate(quickfix.SessionID) {}

func (a app) OnLogon(quickfix.SessionID) {
	a.g.log.Info("gateway logged on")
	// Sending from inside a session callback is avoided.
	go a.g.hub.OnLogon()
}

func (a app) OnLogout(quickfix.SessionID) {
	a.g.log.Info("gateway logged out")
	a.g.hub.OnLogout()
}

func (a app) ToAdmin(*quickfix.Message, quickfix.SessionID)     {}
func (a app) ToApp(*quickfix.Message, quickfix.SessionID) error { return nil }
func (a app) FromAdmin(*quickfix.Message, quickfix.SessionID) quickfix.MessageRejectError {
	return nil
}

func (a app) FromApp(m *quickfix.Message, _ quickfix.SessionID) quickfix.MessageRejectError {
	a.g.hub.OnServerMessage(m)
	return nil
}

// ServeHTTP serves /ws. Each text frame is one FIX message with SOH
// replaced by | (section 8.3).
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Same origin, plus local dev servers (e.g. Vite on another port).
		OriginPatterns: []string{"localhost:*", "127.0.0.1:*"},
	})
	if err != nil {
		return
	}
	conn.SetReadLimit(64 << 10)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var once sync.Once
	c := g.hub.NewClient(func() {
		once.Do(func() {
			_ = conn.Close(websocket.StatusPolicyViolation, "client too slow")
			cancel()
		})
	})
	defer g.hub.Unregister(c)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case fr := <-c.Frames():
				if err := conn.Write(ctx, websocket.MessageText, []byte(fr)); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			_ = conn.CloseNow()
			return
		}
		g.hub.HandleFrame(c, string(data))
	}
}
