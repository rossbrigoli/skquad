// Command browser-proxy is the TG-6 browser egress forward-proxy sidecar
// (docs/tg6-browser-protocol.md §5). Runs beside the browser-service in
// the quarantine pod; Chromium points at it via --proxy-server.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/rossbrigoli/skquad/browser-proxy/internal/proxy"
	"github.com/rossbrigoli/skquad/shared/netguard"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8888"
	}
	// Strict public-only floor: loopback/RFC1918/CGNAT/link-local/
	// metadata/IPv6-ULA are all denied at dial time with IP pinning.
	p := proxy.New(proxy.Config{
		Guard:  &netguard.Guard{},
		Logger: log.New(os.Stdout, "", 0),
	})
	log.Printf("browser-proxy listening on :%s (strict netguard floor, ports 80/443)", port)
	if err := http.ListenAndServe(":"+port, p); err != nil {
		log.Fatalf("browser-proxy: %v", err)
	}
}
