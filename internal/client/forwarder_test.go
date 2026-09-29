package client

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"muse-proxy/internal/proto"
)

func noProxy(_ *http.Request) (*url.URL, error) { return nil, nil }

func upgrade(t *testing.T, w http.ResponseWriter, r *http.Request) *websocket.Conn {
	t.Helper()
	up := websocket.Upgrader{}
	c, err := up.Upgrade(w, r, nil)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	return c
}

func wsTestURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/fwd/testsecret"
}

// TestForwardLoopback: the server opens stream 7 to 127.0.0.1:22 (the real
// local sshd) and the test expects the SSH banner back through the forwarded
// stream. Stream 8 targets a non-allowlisted address and must be refused
// with CLOSE.
func TestForwardLoopback(t *testing.T) {
	var mu sync.Mutex
	var got strings.Builder
	bannerOK := make(chan struct{})
	refusedOK := make(chan struct{})
	var onceB, onceR sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := upgrade(t, w, r)
		defer c.Close()
		// Server side opens the streams.
		c.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FrameOpen, 7, []byte("127.0.0.1:22")))
		c.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FrameOpen, 8, []byte("127.0.0.1:80")))
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
				if id == 7 {
					mu.Lock()
					got.Write(payload)
					hit := strings.Contains(got.String(), "SSH-2.0")
					mu.Unlock()
					if hit {
						onceB.Do(func() { close(bannerOK) })
					}
				}
			case proto.FrameClose:
				if id == 8 {
					onceR.Do(func() { close(refusedOK) })
				}
			}
		}
	}))
	defer srv.Close()

	conn, err := dial(Config{URL: wsTestURL(srv), Proxy: noProxy})
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}
	defer conn.Close()
	f := &forwarder{
		conn:    conn,
		allow:   map[string]bool{"127.0.0.1:22": true},
		dialTO:  5 * time.Second,
		streams: make(map[uint32]*stream),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- f.serve(25 * time.Second) }()

	select {
	case <-bannerOK:
	case err := <-serveErr:
		t.Fatalf("serve exited early: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for SSH banner through forwarded stream")
	}
	select {
	case <-refusedOK:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for CLOSE on non-allowlisted target")
	}
}

// TestStreamIDReuse: two OPENs for the same id must result in exactly one
// live stream; the duplicate is refused with CLOSE.
func TestStreamIDReuse(t *testing.T) {
	var mu sync.Mutex
	closes := 0
	dupClosed := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := upgrade(t, w, r)
		defer c.Close()
		open := proto.Encode(proto.FrameOpen, 7, []byte("127.0.0.1:22"))
		c.WriteMessage(websocket.BinaryMessage, open)
		c.WriteMessage(websocket.BinaryMessage, open) // duplicate id
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
			if typ == proto.FrameClose && id == 7 {
				mu.Lock()
				closes++
				mu.Unlock()
				once.Do(func() { close(dupClosed) })
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
		conn:    conn,
		allow:   map[string]bool{"127.0.0.1:22": true},
		dialTO:  5 * time.Second,
		streams: make(map[uint32]*stream),
	}
	go f.serve(25 * time.Second)

	// The duplicate OPEN must be refused: wait for its CLOSE (deadline, no sleep).
	select {
	case <-dupClosed:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for CLOSE on duplicate OPEN")
	}
	f.mu.Lock()
	n := len(f.streams)
	_, live := f.streams[7]
	f.mu.Unlock()
	if !live || n != 1 {
		t.Fatalf("expected exactly stream 7 live, have %d streams", n)
	}
}

// TestOpenDataOrdering: a DATA frame sent immediately after OPEN must reach
// the target. The injected dial blocks until the test releases it, so the
// stream cannot be registered early; with the old async OPEN handling the
// reader loop would consume (and drop) the DATA while the dial is still
// blocked. After the OPEN is observed inside the dial, the sleep lets the
// reader loop quiesce on the already-buffered DATA — it only needs to
// exceed a microsecond-scale ReadMessage, not win any race.
func TestOpenDataOrdering(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	target := ln.Addr().String()

	got := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 128)
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if n, _ := c.Read(buf); n > 0 {
			got <- append([]byte(nil), buf[:n]...)
		}
	}()

	release := make(chan struct{})
	dialCalled := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := upgrade(t, w, r)
		defer c.Close()
		c.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FrameOpen, 7, []byte(target)))
		c.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FrameData, 7, []byte("SSH-2.0-ordering-probe\r\n")))
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	conn, err := dial(Config{URL: wsTestURL(srv), Proxy: noProxy})
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}
	defer conn.Close()
	f := &forwarder{
		conn:   conn,
		allow:  map[string]bool{target: true},
		dialTO: 5 * time.Second,
		dial: func(network, address string, timeout time.Duration) (net.Conn, error) {
			once.Do(func() { close(dialCalled) })
			<-release
			return net.DialTimeout(network, address, timeout)
		},
		streams: make(map[uint32]*stream),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- f.serve(25 * time.Second) }()

	select {
	case <-dialCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("OPEN was never dispatched to dial")
	}
	time.Sleep(300 * time.Millisecond)
	close(release)

	select {
	case b := <-got:
		if string(b) != "SSH-2.0-ordering-probe\r\n" {
			t.Fatalf("target got %q, want banner", b)
		}
	case err := <-serveErr:
		t.Fatalf("serve exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("DATA sent immediately after OPEN never reached the target (dropped)")
	}
}

// TestDialSendsBearerHeader pins the wire side of the header form: with
// Config.Secret set the Authorization header must be present on every dial
// (including reconnects, which go through this same function), and with it
// unset no header may be sent at all.
func TestDialSendsBearerHeader(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		upgrade(t, w, r).Close()
	}))
	defer srv.Close()
	u := wsTestURL(srv)

	if c, err := dial(Config{URL: u, Proxy: noProxy}); err != nil {
		t.Fatalf("dial without secret: %v", err)
	} else {
		c.Close()
	}
	if got[0] != "" {
		t.Fatalf("no secret configured but header was %q", got[0])
	}

	if c, err := dial(Config{URL: u, Proxy: noProxy, Secret: "s3cret"}); err != nil {
		t.Fatalf("dial with secret: %v", err)
	} else {
		c.Close()
	}
	if got[1] != "Bearer s3cret" {
		t.Fatalf("header = %q, want %q", got[1], "Bearer s3cret")
	}
}
