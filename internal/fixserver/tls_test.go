package fixserver

import (
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"
	qconfig "github.com/quickfixgo/quickfix/config"

	"github.com/quynv/mock-fixser/internal/config"
	"github.com/quynv/mock-fixser/internal/tlsgen"
)

func tlsServerConfig(t *testing.T, clientPort, monitorPort int, certs, clientAuth string) *config.Config {
	t.Helper()
	y := fmt.Sprintf(`
server:
  fix:
    listen: 127.0.0.1:%d
    data_dictionary: %s
    tls: { cert_file: %s, key_file: %s, client_auth: %s, client_ca_file: %s }
    sessions:
      - { sender_comp_id: MOCKSRV, target_comp_id: CLIENT1 }
  monitor: { enabled: true, listen: 127.0.0.1:%d }
symbols: []
`, clientPort, dictPath, filepath.Join(certs, "server.crt"), filepath.Join(certs, "server.key"), clientAuth,
		filepath.Join(certs, "ca.crt"), monitorPort)
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// dialTLS connects like the postprime gateways: SocketUseSSL with a client
// certificate and no server verification.
func dialTLS(t *testing.T, port int, certs string, clientCert bool) *testClient {
	t.Helper()
	settings := quickfix.NewSettings()
	ss := sessionSettings("CLIENT1", "MOCKSRV")
	ss.Set(qconfig.SocketConnectHost, "127.0.0.1")
	ss.Set(qconfig.SocketConnectPort, strconv.Itoa(port))
	ss.Set(qconfig.ReconnectInterval, "1")
	ss.Set(qconfig.SocketUseSSL, "Y")
	ss.Set(qconfig.SocketInsecureSkipVerify, "Y")
	if clientCert {
		ss.Set(qconfig.SocketCertificateFile, filepath.Join(certs, "client.crt"))
		ss.Set(qconfig.SocketPrivateKeyFile, filepath.Join(certs, "client.key"))
	}
	id, err := settings.AddSession(ss)
	if err != nil {
		t.Fatal(err)
	}
	app := &clientApp{testApp: newTestApp(), logout: make(chan quickfix.SessionID, 10), msgs: make(chan *quickfix.Message, 10)}
	app.loggedOn = make(chan quickfix.SessionID, 10)
	ini, err := quickfix.NewInitiator(app, quickfix.NewMemoryStoreFactory(), settings, quickfix.NewNullLogFactory())
	if err != nil {
		t.Fatal(err)
	}
	if err := ini.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ini.Stop)
	return &testClient{testApp: app.testApp, ini: ini, loggedOf: app.logout, appMsgs: app.msgs, id: id}
}

func TestTLSAcceptor(t *testing.T) {
	certs := t.TempDir()
	if err := tlsgen.Generate(certs, []string{"127.0.0.1"}, time.Hour); err != nil {
		t.Fatal(err)
	}

	t.Run("client certificate required", func(t *testing.T) {
		cp, mp := freePort(t), freePort(t)
		startServer(t, tlsServerConfig(t, cp, mp, certs, "require"))
		dialTLS(t, cp, certs, true).waitLogon(t)
	})
	t.Run("missing client certificate is refused", func(t *testing.T) {
		cp, mp := freePort(t), freePort(t)
		startServer(t, tlsServerConfig(t, cp, mp, certs, "require"))
		dialTLS(t, cp, certs, false).expectNoLogon(t)
	})
	t.Run("plain TCP is refused", func(t *testing.T) {
		cp, mp := freePort(t), freePort(t)
		startServer(t, tlsServerConfig(t, cp, mp, certs, "none"))
		dialClient(t, cp, "CLIENT1", nil).expectNoLogon(t)
	})
	t.Run("monitor stays plain", func(t *testing.T) {
		cp, mp := freePort(t), freePort(t)
		startServer(t, tlsServerConfig(t, cp, mp, certs, "none"))
		dialClient(t, mp, "UI_MONITOR", nil).waitLogon(t)
	})
}
