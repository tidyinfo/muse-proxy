package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"muse-proxy/internal/proto"
)

func openShellPayload(t *testing.T) []byte {
	t.Helper()
	p, err := json.Marshal(map[string]uint32{"cols": 80, "rows": 24})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestOpenShell starts a real pty shell through the forwarder: the server
// sends OPEN_SHELL, then a command; the shell's output must come back as
// DATA on the same stream.
func TestOpenShell(t *testing.T) {
	markerSeen := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var out strings.Builder

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := upgrade(t, w, r)
		defer c.Close()
		c.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FrameOpenShell, 9, openShellPayload(t)))
		time.Sleep(time.Second) // let the shell start; pty buffers anyway
		c.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FrameData, 9, []byte("echo SHELLMARKER123\n")))
		// A resize mid-session must not break the stream.
		time.Sleep(time.Second)
		c.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FrameWinch, 9, []byte("100x30")))
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.BinaryMessage {
				continue
			}
			typ, id, payload, ok := proto.Decode(msg)
			if !ok {
				continue
			}
			switch typ {
			case proto.FramePing:
				c.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FramePong, 0, nil))
			case proto.FrameData:
				if id == 9 {
					mu.Lock()
					out.Write(payload)
					hit := strings.Contains(out.String(), "SHELLMARKER123")
					mu.Unlock()
					if hit {
						once.Do(func() { close(markerSeen) })
					}
				}
			}
		}
	}))
	defer srv.Close()

	conn, err := dial(Config{URL: wsTestURL(srv), Proxy: noProxy})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	f := &forwarder{
		conn:       conn,
		allow:      map[string]bool{},
		allowShell: true,
		dialTO:     5 * time.Second,
		streams:    make(map[uint32]*stream),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- f.serve(25 * time.Second) }()

	select {
	case <-markerSeen:
	case err := <-serveErr:
		t.Fatalf("serve exited early: %v", err)
	case <-time.After(20 * time.Second):
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("timed out waiting for shell output; got %q", out.String())
	}

	// Exiting the shell must tear the stream down with CLOSE.
	f.mu.Lock()
	_, live := f.streams[9]
	f.mu.Unlock()
	if !live {
		t.Fatal("shell stream 9 should be live")
	}
}

// TestOpenShellDisabled: with DisableShell the client refuses OPEN_SHELL.
func TestOpenShellDisabled(t *testing.T) {
	closed := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := upgrade(t, w, r)
		defer c.Close()
		c.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FrameOpenShell, 9, openShellPayload(t)))
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.BinaryMessage {
				continue
			}
			typ, id, _, ok := proto.Decode(msg)
			if !ok {
				continue
			}
			if typ == proto.FramePing {
				c.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FramePong, 0, nil))
			}
			if typ == proto.FrameClose && id == 9 {
				once.Do(func() { close(closed) })
			}
		}
	}))
	defer srv.Close()

	conn, err := dial(Config{URL: wsTestURL(srv), Proxy: noProxy})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	f := &forwarder{
		conn:       conn,
		allow:      map[string]bool{},
		allowShell: false,
		dialTO:     5 * time.Second,
		streams:    make(map[uint32]*stream),
	}
	go f.serve(25 * time.Second)

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("expected CLOSE for OPEN_SHELL with DisableShell")
	}
	f.mu.Lock()
	n := len(f.streams)
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("expected no streams, have %d", n)
	}
}
