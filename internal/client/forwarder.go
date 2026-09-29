// Package client implements the muse-proxy client: it dials out a single
// WebSocket to the server and multiplexes forwarded TCP streams over it.
//
// The client never listens. Every stream -- TCP forward or web shell -- is
// initiated by the server.
package client

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"muse-proxy/internal/proto"
)

// Config configures the client.
type Config struct {
	// URL is the server endpoint, e.g. wss://example.com/fwd/<secret>.
	URL string
	// Allow is the set of local targets ("host:port") a stream may be
	// forwarded to. Anything else is refused with CLOSE.
	Allow map[string]bool
	// Proxy maps outgoing requests to a proxy URL, or nil for direct.
	// Use ProxyFromEnvironment for HTTPS_PROXY/NO_PROXY support.
	Proxy func(*http.Request) (*url.URL, error)
	// PingInterval is the WebSocket keepalive interval. Default 25s.
	PingInterval time.Duration
	// DialTimeout caps the local TCP dial per stream. Default 10s.
	DialTimeout time.Duration
	// DisableShell refuses OPEN_SHELL requests (no pty sessions).
	DisableShell bool
	// Secret, when set, is sent as `Authorization: Bearer` instead of being
	// part of URL. The path form is the default because a reverse proxy can
	// turn access logging off per-location without inspecting headers; the
	// header form exists for operators who would rather the secret never
	// appear in a URL at all. Both are accepted by the server.
	Secret string
}

func (c Config) withDefaults() Config {
	if c.PingInterval <= 0 {
		c.PingInterval = 25 * time.Second
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = 10 * time.Second
	}
	return c
}

// ProxyFromEnvironment honors HTTPS_PROXY/HTTP_PROXY/NO_PROXY for ws/wss
// dials. Go's http.ProxyFromEnvironment only matches http/https schemes, so
// ws/wss are mapped to http/https for the lookup.
func ProxyFromEnvironment() func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		if req.URL.Scheme == "ws" || req.URL.Scheme == "wss" {
			r2 := new(http.Request)
			*r2 = *req
			u := *req.URL
			if u.Scheme == "ws" {
				u.Scheme = "http"
			} else {
				u.Scheme = "https"
			}
			r2.URL = &u
			return http.ProxyFromEnvironment(r2)
		}
		return http.ProxyFromEnvironment(req)
	}
}

type stream struct {
	id    uint32
	tcp   net.Conn
	shell *shellSession
}

type forwarder struct {
	conn       *websocket.Conn
	allow      map[string]bool
	allowShell bool
	dialTO     time.Duration
	// dial dials the local target for a new stream; nil means
	// net.DialTimeout. Tests inject a blocking dial to pin down the
	// OPEN-then-DATA ordering.
	dial     func(network, address string, timeout time.Duration) (net.Conn, error)
	writeMu  sync.Mutex // serializes all websocket writes
	mu       sync.Mutex
	streams  map[uint32]*stream
	lastPong time.Time
}

func (f *forwarder) sendFrame(typ byte, id uint32, payload []byte) error {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	f.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return f.conn.WriteMessage(websocket.BinaryMessage, proto.Encode(typ, id, payload))
}

// closeStream is idempotent: closing an unknown id is a no-op.
func (f *forwarder) closeStream(id uint32) {
	f.mu.Lock()
	s, ok := f.streams[id]
	if ok {
		delete(f.streams, id)
	}
	f.mu.Unlock()
	if ok {
		if s.tcp != nil {
			s.tcp.Close()
		}
		if s.shell != nil {
			s.shell.close()
		}
	}
}

func (f *forwarder) handleOpen(id uint32, target string) {
	if !f.allow[target] {
		log.Printf("stream %d: target %q not allowlisted, refusing", id, target)
		f.sendFrame(proto.FrameClose, id, nil)
		return
	}
	dial := f.dial
	if dial == nil {
		dial = net.DialTimeout
	}
	tcp, err := dial("tcp", target, f.dialTO)
	if err != nil {
		log.Printf("stream %d: dial %s: %v", id, target, err)
		f.sendFrame(proto.FrameClose, id, nil)
		return
	}
	f.mu.Lock()
	if _, exists := f.streams[id]; exists {
		f.mu.Unlock()
		tcp.Close()
		f.sendFrame(proto.FrameClose, id, nil)
		return
	}
	f.streams[id] = &stream{id: id, tcp: tcp}
	f.mu.Unlock()
	log.Printf("stream %d: forwarding to %s", id, target)
	go f.pump(id, tcp)
}

// pump copies tcp -> websocket until EOF/error, then tears the stream down.
func (f *forwarder) pump(id uint32, tcp net.Conn) {
	defer f.closeStream(id)
	defer f.sendFrame(proto.FrameClose, id, nil)
	buf := make([]byte, 32*1024)
	for {
		n, err := tcp.Read(buf)
		if n > 0 {
			if werr := f.sendFrame(proto.FrameData, id, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (f *forwarder) handleData(id uint32, payload []byte) {
	f.mu.Lock()
	s, ok := f.streams[id]
	f.mu.Unlock()
	if !ok {
		return
	}
	var err error
	if s.tcp != nil {
		s.tcp.SetWriteDeadline(time.Now().Add(30 * time.Second))
		_, err = s.tcp.Write(payload)
	} else if s.shell != nil {
		err = s.shell.write(payload)
	}
	if err != nil {
		f.closeStream(id)
		f.sendFrame(proto.FrameClose, id, nil)
	}
}

// handleWinch applies a terminal resize ("COLSxROWS") to a shell stream.
func (f *forwarder) handleWinch(id uint32, payload []byte) {
	var cols, rows uint32
	if _, err := fmt.Sscanf(string(payload), "%dx%d", &cols, &rows); err != nil || cols == 0 || rows == 0 {
		return
	}
	f.mu.Lock()
	s, ok := f.streams[id]
	f.mu.Unlock()
	if ok && s.shell != nil {
		s.shell.winch(cols, rows)
	}
}

// serve runs the reader loop until the connection fails. It sends periodic
// PINGs and kills the connection if PONGs stop arriving.
func (f *forwarder) serve(pingInterval time.Duration) error {
	f.lastPong = time.Now()
	defer func() {
		f.mu.Lock()
		for id, s := range f.streams {
			if s.tcp != nil {
				s.tcp.Close()
			}
			if s.shell != nil {
				s.shell.close()
			}
			delete(f.streams, id)
		}
		f.mu.Unlock()
	}()

	pingDone := make(chan struct{})
	defer close(pingDone)
	go func() {
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-t.C:
				if err := f.sendFrame(proto.FramePing, 0, nil); err != nil {
					return
				}
				f.mu.Lock()
				stale := time.Since(f.lastPong) > 3*pingInterval
				f.mu.Unlock()
				if stale {
					f.conn.Close()
					return
				}
			}
		}
	}()

	for {
		mt, msg, err := f.conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("ws read: %w", err)
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		typ, id, payload, ok := proto.Decode(msg)
		if !ok {
			continue
		}
		switch typ {
		case proto.FrameOpen:
			// Synchronous on purpose: the stream must be registered
			// before any later frame for this id is handled. A DATA
			// frame (e.g. an SSH client banner) can arrive in the same
			// read batch as the OPEN; handling OPEN in a goroutine
			// would race the dial and silently drop that first DATA.
			f.handleOpen(id, string(payload))
		case proto.FrameData:
			f.handleData(id, payload)
		case proto.FrameClose:
			f.closeStream(id)
		case proto.FramePong:
			f.mu.Lock()
			f.lastPong = time.Now()
			f.mu.Unlock()
		case proto.FrameOpenShell:
			// Same ordering requirement as FrameOpen: register the pty
			// stream before later DATA (early keystrokes) is handled.
			f.handleOpenShell(id, payload)
		case proto.FrameWinch:
			f.handleWinch(id, payload)
		case proto.FramePing:
			f.sendFrame(proto.FramePong, 0, nil)
		}
	}
}

func dial(cfg Config) (*websocket.Conn, error) {
	d := websocket.Dialer{HandshakeTimeout: 20 * time.Second, Proxy: cfg.Proxy}
	// Built per dial, never shared: a reconnect must not reuse a header map a
	// previous handshake touched, and the secret must not outlive this call.
	var hdr http.Header
	if cfg.Secret != "" {
		hdr = http.Header{"Authorization": {"Bearer " + cfg.Secret}}
	}
	c, _, err := d.Dial(cfg.URL, hdr)
	return c, err
}

// Run holds the multiplexed connection, reconnecting with backoff until ctx
// is cancelled.
func Run(ctx context.Context, cfg Config) {
	cfg = cfg.withDefaults()
	backoff := 5 * time.Second
	const maxBackoff = 60 * time.Second
	for {
		log.Printf("dialing %s ...", redactURL(cfg.URL))
		c, err := dial(cfg)
		if err != nil {
			log.Printf("dial failed: %v; retry in %s", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		log.Printf("websocket connected (muse-proxy wire protocol v%d)", proto.Version)
		backoff = 5 * time.Second
		f := &forwarder{
			conn:       c,
			allow:      cfg.Allow,
			allowShell: !cfg.DisableShell,
			dialTO:     cfg.DialTimeout,
			streams:    make(map[uint32]*stream),
		}
		serveDone := make(chan error, 1)
		go func() { serveDone <- f.serve(cfg.PingInterval) }()
		endConn := func() { c.Close() }
		select {
		case <-ctx.Done():
			endConn()
			<-serveDone
			return
		case err := <-serveDone:
			log.Printf("connection lost: %v", err)
		}
		endConn()
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// redactURL keeps host/path shape in logs while hiding the secret.
func redactURL(wsURL string) string {
	u, err := url.Parse(wsURL)
	if err != nil {
		return "wss://<unparseable>"
	}
	return u.Scheme + "://" + u.Host + "/fwd/<secret>"
}
