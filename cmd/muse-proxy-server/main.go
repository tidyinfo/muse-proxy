// Command muse-proxy-server runs on the public host: it validates the
// client secret, accepts TCP on a loopback port, and bridges each connection
// to a stream on the client's outbound WebSocket.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"muse-proxy/internal/server"
)

func readSecretFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read secret-file: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if v, ok := strings.CutPrefix(line, "FWD_SECRET="); ok {
			return strings.TrimSpace(v)
		}
		return line // raw secret
	}
	log.Fatalf("empty secret file %s", path)
	return ""
}

// parseAllow splits a comma-separated allowlist into a set.
func parseAllow(s string) map[string]bool {
	m := make(map[string]bool)
	for _, tg := range strings.Split(s, ",") {
		if tg = strings.TrimSpace(tg); tg != "" {
			m[tg] = true
		}
	}
	return m
}

func main() {
	secretFlag := flag.String("secret", "", "bearer secret (prefer -secret-file)")
	secretFile := flag.String("secret-file", "", "file containing the secret (FWD_SECRET=... or raw)")
	httpAddr := flag.String("http", "127.0.0.1:18080", "http bind for the /fwd/ websocket endpoint (behind nginx)")
	tcpAddr := flag.String("tcp", "127.0.0.1:2222", "tcp listen addr for forwarded connections (keep on loopback)")
	target := flag.String("target", "127.0.0.1:22", "client-side forward target sent in OPEN frames")
	flag.Parse()

	secret := *secretFlag
	if *secretFile != "" {
		secret = readSecretFile(*secretFile)
	}
	if secret == "" {
		log.Fatal("one of -secret or -secret-file is required")
	}

	srv, err := server.New(server.Config{
		Secret:     secret,
		ListenAddr: *tcpAddr,
		Target:     *target,
	})
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		if err := srv.Run(ctx); err != nil {
			log.Fatalf("tcp bridge: %v", err)
		}
	}()
	httpSrv := &http.Server{Addr: *httpAddr, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		httpSrv.Close()
	}()
	log.Printf("websocket endpoint on http://%s/fwd/<secret>", *httpAddr)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
