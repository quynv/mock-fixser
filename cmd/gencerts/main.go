// Command gencerts writes a self-signed CA, a server certificate and a
// client certificate for testing mockfix over TLS (server.fix.tls).
//
//	go run ./cmd/gencerts -dir certs -hosts localhost,127.0.0.1,mockfix
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/quynv/mock-fixser/internal/tlsgen"
)

func main() {
	dir := flag.String("dir", "certs", "output directory")
	hosts := flag.String("hosts", "localhost,127.0.0.1", "server certificate DNS names and IPs")
	days := flag.Int("days", 825, "validity in days")
	flag.Parse()
	if err := tlsgen.Generate(*dir, strings.Split(*hosts, ","), time.Duration(*days)*24*time.Hour); err != nil {
		fmt.Fprintln(os.Stderr, "gencerts:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote ca.crt, server.crt/key and client.crt/key to %s\n", *dir)
}
