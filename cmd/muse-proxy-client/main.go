// Command muse-proxy-client runs on the egress-only host: it dials out a
// single WebSocket to the server and multiplexes forwarded TCP streams over
// it. See PROTOCOL.md for the wire protocol.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"muse-proxy/internal/client"
)

// Build stamps, set by the release ldflags in .goreleaser.yaml. They must
// exist as package-level vars or `-X` silently does nothing: the linker
// ignores a -X naming a symbol the package never declared, and the release
// ships unversioned binaries with no error anywhere.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func readURLFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read url-file: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "FWD_URL="); ok {
			return strings.TrimSpace(v)
		}
	}
	log.Fatalf("no FWD_URL= line in %s", path)
	return ""
}

// readSecretFile returns the FWD_SECRET= value, or "" when the file has none.
// Unlike readURLFile a missing key is not fatal: the same file may carry only
// FWD_URL, with the secret embedded in the path the way it always was.
func readSecretFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read secret-file: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "FWD_SECRET="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func main() {
	urlFlag := flag.String("url", "", "wss url, e.g. wss://host/fwd/<secret>")
	urlFile := flag.String("url-file", "", "read wss url from a file (FWD_URL=... line); keeps the secret out of argv")
	secretFile := flag.String("secret-file", "", "optional; read FWD_SECRET= from a file and send it as `Authorization: Bearer` instead of putting it in the URL path")
	allowFlag := flag.String("allow", "127.0.0.1:22", "comma-separated allowlisted local targets")
	proxyFlag := flag.String("proxy", "", "http proxy url for CONNECT, e.g. http://user:pass@host:3128")
	pingInterval := flag.Duration("ping-interval", 25*time.Second, "websocket keepalive ping interval")
	disableShell := flag.Bool("disable-shell", false, "refuse OPEN_SHELL requests (no pty sessions); the TCP allowlist is unaffected")
	versionFlag := flag.Bool("version", false, "print build version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("%s %s (commit %s, built %s)\n", "muse-proxy-client", version, commit, date)
		return
	}

	urlVal := *urlFlag
	var secretVal string
	if *urlFile != "" {
		urlVal = readURLFile(*urlFile)
	}
	if *secretFile != "" {
		secretVal = readSecretFile(*secretFile)
	}
	if urlVal == "" {
		log.Fatal("one of -url or -url-file is required")
	}
	allow := make(map[string]bool)
	for _, tg := range strings.Split(*allowFlag, ",") {
		if tg = strings.TrimSpace(tg); tg != "" {
			allow[tg] = true
		}
	}
	if len(allow) == 0 {
		log.Fatal("allow list is empty")
	}

	var proxy func(*http.Request) (*url.URL, error)
	if *proxyFlag != "" {
		pu, err := url.Parse(*proxyFlag)
		if err != nil {
			log.Fatalf("bad proxy url: %v", err)
		}
		proxy = http.ProxyURL(pu)
	} else {
		proxy = client.ProxyFromEnvironment()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	client.Run(ctx, client.Config{
		URL:          urlVal,
		Secret:       secretVal,
		Allow:        allow,
		DisableShell: *disableShell,
		Proxy:        proxy,
		PingInterval: *pingInterval,
	})
}
