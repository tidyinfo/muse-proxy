package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"muse-proxy/internal/proto"
)

func TestSecretRejected(t *testing.T) {
	s, err := New(Config{Secret: "correct-secret", ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	base := "ws" + strings.TrimPrefix(ts.URL, "http")

	for _, path := range []string{"/fwd/wrong", "/fwd/", "/nope"} {
		_, _, err := websocket.DefaultDialer.Dial(base+path, nil)
		if err == nil {
			t.Fatalf("dial to %s should have been rejected", path)
		}
	}
	// Correct secret must upgrade.
	ws, _, err := websocket.DefaultDialer.Dial(base+"/fwd/correct-secret", nil)
	if err != nil {
		t.Fatalf("dial with correct secret: %v", err)
	}
	ws.Close()
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{ListenAddr: "127.0.0.1:0"}); err == nil {
		t.Fatal("expected error for empty secret")
	}
	if _, err := New(Config{Secret: "x"}); err == nil {
		t.Fatal("expected error for empty listen addr")
	}
}

// TestBridgeRoundTrip wires a real TCP client through the server to a fake
// client (the test.s websocket) (the test's websocket), verifying OPEN/DATA/CLOSE framing.
func TestBridgeRoundTrip(t *testing.T) {
	s, err := New(Config{
		Secret:     "s3cret",
		ListenAddr: "127.0.0.1:0",
		Target:     "127.0.0.1:22",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for s.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.Addr() == "" {
		t.Fatal("server TCP listener did not start")
	}

	// Fake client (the test.s websocket).
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/fwd/s3cret"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))

	// Synchronize with the server session: a answered PING proves attach()
	// is running and this connection is registered, so the TCP dial below
	// cannot race it.
	if err := ws.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FramePing, 0, nil)); err != nil {
		t.Fatal(err)
	}
	readFrame := func() (byte, uint32, []byte) {
		t.Helper()
		mt, msg, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		if mt != websocket.BinaryMessage {
			t.Fatalf("expected binary message, got %d", mt)
		}
		typ, id, payload, ok := proto.Decode(msg)
		if !ok {
			t.Fatal("short frame")
		}
		return typ, id, payload
	}
	if typ, _, _ := readFrame(); typ != proto.FramePong {
		t.Fatalf("expected PONG, got 0x%02x", typ)
	}

	// Simulate a user connecting to the forwarded port.
	tc, err := net.DialTimeout("tcp", s.Addr(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	if _, err := tc.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}

	typ, id, payload := readFrame()
	if typ != proto.FrameOpen {
		t.Fatalf("expected OPEN, got 0x%02x", typ)
	}
	if id%2 == 0 {
		t.Fatalf("server stream ids should be odd, got %d", id)
	}
	if string(payload) != "127.0.0.1:22" {
		t.Fatalf("OPEN target = %q", payload)
	}

	typ, gotID, payload := readFrame()
	if typ != proto.FrameData || gotID != id || string(payload) != "hello" {
		t.Fatalf("DATA mismatch: typ=0x%02x id=%d payload=%q", typ, gotID, payload)
	}

	// Server -> TCP direction.
	if err := ws.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FrameData, id, []byte("world"))); err != nil {
		t.Fatal(err)
	}
	tc.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 16)
	n, err := tc.Read(buf)
	if err != nil || string(buf[:n]) != "world" {
		t.Fatalf("tcp read: n=%d err=%v", n, err)
	}

	// PING -> PONG.
	if err := ws.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FramePing, 0, nil)); err != nil {
		t.Fatal(err)
	}
	typ, _, _ = readFrame()
	if typ != proto.FramePong {
		t.Fatalf("expected PONG, got 0x%02x", typ)
	}

	// CLOSE tears down the TCP side.
	if err := ws.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FrameClose, id, nil)); err != nil {
		t.Fatal(err)
	}
	tc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := tc.Read(buf); err == nil {
		t.Fatal("expected EOF after CLOSE")
	}
}

func TestTCPRefusedWithoutClient(t *testing.T) {
	s, err := New(Config{Secret: "s3cret", ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for s.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	tc, err := net.DialTimeout("tcp", s.Addr(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	// No client connected: the server must refuse fast (close), not hang.
	tc.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	if _, err := tc.Read(buf); err == nil {
		t.Fatal("expected the connection to be refused")
	}
}

// TestShellPage serves the embedded terminal HTML.
func TestShellPage(t *testing.T) {
	s, err := New(Config{Secret: "s3cret", ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/shell/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /shell/ = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "xterm") {
		t.Fatal("shell page does not look like the terminal")
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}

	resp2, err := http.Get(ts.URL + "/shell/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /shell/nope = %d, want 404", resp2.StatusCode)
	}
}

// TestShellBridge wires a fake browser through the server to a fake remote client
// client, verifying OPEN_SHELL/DATA/WINCH/CLOSE across the bridge.
func TestShellBridge(t *testing.T) {
	s, err := New(Config{Secret: "s3cret", ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for s.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	base := "ws" + strings.TrimPrefix(ts.URL, "http")

	// Browser with wrong secret is rejected.
	if _, _, err := websocket.DefaultDialer.Dial(base+"/shell/ws/wrong", nil); err == nil {
		t.Fatal("browser dial with wrong secret should fail")
	}

	// Fake client (the test.s websocket).
	clientWS, _, err := websocket.DefaultDialer.Dial(base+"/fwd/s3cret", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientWS.Close()
	clientWS.SetReadDeadline(time.Now().Add(5 * time.Second))

	// Fake browser page.
	browserWS, _, err := websocket.DefaultDialer.Dial(base+"/shell/ws/s3cret", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer browserWS.Close()
	browserWS.SetReadDeadline(time.Now().Add(5 * time.Second))

	readFrame := func(ws *websocket.Conn) (byte, uint32, []byte) {
		t.Helper()
		mt, msg, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		if mt != websocket.BinaryMessage {
			t.Fatalf("expected binary, got %d", mt)
		}
		typ, id, payload, ok := proto.Decode(msg)
		if !ok {
			t.Fatal("short frame")
		}
		return typ, id, payload
	}

	// Browser opens a shell (its own frame id 1).
	openPayload := []byte(`{"cols":80,"rows":24}`)
	if err := browserWS.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FrameOpenShell, 1, openPayload)); err != nil {
		t.Fatal(err)
	}
	typ, id, payload := readFrame(clientWS)
	if typ != proto.FrameOpenShell {
		t.Fatalf("expected OPEN_SHELL, got 0x%02x", typ)
	}
	if id%2 == 0 {
		t.Fatalf("shell stream ids should be odd, got %d", id)
	}
	if string(payload) != string(openPayload) {
		t.Fatalf("OPEN_SHELL payload = %q", payload)
	}

	// Client -> browser: pty output arrives with the browser's id.
	if err := clientWS.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FrameData, id, []byte("hello-shell"))); err != nil {
		t.Fatal(err)
	}
	typ, bid, payload := readFrame(browserWS)
	if typ != proto.FrameData || bid != 1 || string(payload) != "hello-shell" {
		t.Fatalf("browser DATA mismatch: typ=0x%02x id=%d payload=%q", typ, bid, payload)
	}

	// Browser -> client: keystrokes and resize.
	if err := browserWS.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FrameData, 1, []byte("ls\n"))); err != nil {
		t.Fatal(err)
	}
	typ, gotID, payload := readFrame(clientWS)
	if typ != proto.FrameData || gotID != id || string(payload) != "ls\n" {
		t.Fatalf("client DATA mismatch: typ=0x%02x id=%d payload=%q", typ, gotID, payload)
	}
	if err := browserWS.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FrameWinch, 1, []byte("100x30"))); err != nil {
		t.Fatal(err)
	}
	typ, gotID, payload = readFrame(clientWS)
	if typ != proto.FrameWinch || gotID != id || string(payload) != "100x30" {
		t.Fatalf("client WINCH mismatch: typ=0x%02x id=%d payload=%q", typ, gotID, payload)
	}

	// Client CLOSE tears down the browser side.
	if err := clientWS.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FrameClose, id, nil)); err != nil {
		t.Fatal(err)
	}
	browserWS.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := browserWS.ReadMessage(); err == nil {
		t.Fatal("expected browser connection to close after CLOSE")
	}
}

// TestShellWithoutClient: a browser session with no client fails fast.
func TestShellWithoutClient(t *testing.T) {
	s, err := New(Config{Secret: "s3cret", ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	base := "ws" + strings.TrimPrefix(ts.URL, "http")

	browserWS, _, err := websocket.DefaultDialer.Dial(base+"/shell/ws/s3cret", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer browserWS.Close()
	if err := browserWS.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FrameOpenShell, 1, []byte(`{"cols":80,"rows":24}`))); err != nil {
		t.Fatal(err)
	}
	browserWS.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := browserWS.ReadMessage(); err == nil {
		t.Fatal("expected fast failure with no client connected")
	}
}
