// Package server implements the muse-proxy server side (public host): it
// validates the client secret, accepts TCP on a loopback port, and bridges
// each connection to a stream on the client's outbound WebSocket.
package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"muse-proxy/internal/proto"
	"muse-proxy/web"
)

// Config configures the server.
type Config struct {
	// Secret is the bearer token the client presents in the URL path.
	Secret string
	// ListenAddr is the TCP address for forwarded connections,
	// e.g. "127.0.0.1:2222". Keep it on loopback.
	ListenAddr string
	// Target is the "host:port" sent in OPEN frames, telling the client
	// where to forward each stream, e.g. "127.0.0.1:22".
	Target string
	// PingTimeout closes the session after this long without any frame.
	// Default 5 minutes.
	PingTimeout time.Duration
	// Logger, if nil, uses the standard logger.
	Logger *log.Logger
}

func (c Config) withDefaults() Config {
	if c.PingTimeout <= 0 {
		c.PingTimeout = 5 * time.Minute
	}
	if c.Target == "" {
		c.Target = "127.0.0.1:22"
	}
	return c
}

// stream is one multiplexed channel: either a TCP forward or a web shell.
type stream struct {
	id    uint32
	tcp   net.Conn
	shell *shellSession
}

// shellSession bridges one browser WebSocket to a client pty stream.
type shellSession struct {
	browser   *websocket.Conn
	browserID uint32 // frame id the browser page uses
	writeMu   sync.Mutex
}

// sendToBrowser writes one frame to the browser page, serialized.
func (sh *shellSession) sendToBrowser(typ byte, payload []byte) error {
	sh.writeMu.Lock()
	defer sh.writeMu.Unlock()
	sh.browser.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return sh.browser.WriteMessage(websocket.BinaryMessage, proto.Encode(typ, sh.browserID, payload))
}

// Server bridges one client's WebSocket to a local TCP listener.
type Server struct {
	cfg      Config
	log      *log.Logger
	upgrader websocket.Upgrader

	mu      sync.Mutex
	ws      *websocket.Conn
	writeMu sync.Mutex // serializes all websocket writes
	streams map[uint32]*stream
	nextID  uint32 // odd ids only
	gen     uint64 // session generation, bumped on every attach

	// testBeforeSessionCleanup is a test-only hook run at the start of an
	// attach's teardown. Stored as atomic.Pointer because attach runs on a
	// connection goroutine while tests may clear it from another one.
	// Always nil in production.
	testBeforeSessionCleanup atomic.Pointer[func()]

	ln   net.Listener
	addr string
}

// New validates cfg and returns a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Secret == "" {
		return nil, fmt.Errorf("secret is required")
	}
	if cfg.ListenAddr == "" {
		return nil, fmt.Errorf("listen addr is required")
	}
	cfg = cfg.withDefaults()
	lg := cfg.Logger
	if lg == nil {
		lg = log.Default()
	}
	return &Server{cfg: cfg, log: lg, streams: make(map[uint32]*stream)}, nil
}

// Handler serves /fwd/<secret> (client), /shell/ (web terminal page) and
// /shell/ws/<secret> (browser sessions).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/fwd/", s.handleFwd)
	mux.HandleFunc("/shell/ws/", s.handleShellWS)
	mux.HandleFunc("/shell/", s.handleShellPage)
	return mux
}

func (s *Server) handleFwd(w http.ResponseWriter, r *http.Request) {
	got := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/fwd/"), "/", 2)[0]
	if !s.secretOK(got, bearer(r)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	c, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.log.Printf("client connected from %s (muse-proxy wire protocol v%d)", r.RemoteAddr, proto.Version)
	s.attach(c)
	s.log.Printf("client disconnected")
}

// bearer returns the token from an `Authorization: Bearer` header, or "".
func bearer(r *http.Request) string {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// secretOK reports whether either carrier holds the configured secret.
//
// The two are alternative carriers for the same value, not a hierarchy, so
// either one matching is sufficient and there is no precedence to reason
// about. Both comparisons are evaluated before the result is combined, so
// neither branch can be skipped by the other's outcome.
func (s *Server) secretOK(path, header string) bool {
	okPath := subtle.ConstantTimeCompare([]byte(path), []byte(s.cfg.Secret)) == 1
	okHeader := subtle.ConstantTimeCompare([]byte(header), []byte(s.cfg.Secret)) == 1
	return okPath || okHeader
}

// handleShellPage serves the embedded web terminal. Put authentication in
// front of it at the reverse proxy (see docs/SECURITY.md).
func (s *Server) handleShellPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/shell/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(web.ShellHTML)
}

func (s *Server) handleShellWS(w http.ResponseWriter, r *http.Request) {
	got := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/shell/ws/"), "/", 2)[0]
	if !s.secretOK(got, bearer(r)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	c, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.log.Printf("web shell connected from %s", r.RemoteAddr)
	s.shellBridge(c)
	s.log.Printf("web shell disconnected")
}

// shellBridge maps one browser WebSocket to a pty stream on the client.
// The browser's first frame must be OPEN_SHELL; after that its frames are
// forwarded to the stream (frame ids on the browser side are ignored).
func (s *Server) shellBridge(browser *websocket.Conn) {
	defer browser.Close()
	s.mu.Lock()
	ws := s.ws
	s.mu.Unlock()
	if ws == nil {
		return // no client connected: fail fast
	}
	browser.SetReadDeadline(time.Now().Add(s.cfg.PingTimeout))
	mt, msg, err := browser.ReadMessage()
	if err != nil {
		return
	}
	typ, browserID, payload, ok := proto.Decode(msg)
	if !ok || mt != websocket.BinaryMessage || typ != proto.FrameOpenShell {
		return
	}
	id := atomic.AddUint32(&s.nextID, 2) | 1 // odd ids: 1, 3, 5, ...
	sh := &shellSession{browser: browser, browserID: browserID}
	s.mu.Lock()
	s.streams[id] = &stream{id: id, shell: sh}
	s.mu.Unlock()
	defer s.dropStream(id)
	defer s.sendFrame(proto.FrameClose, id, nil)

	if err := s.sendFrame(proto.FrameOpenShell, id, payload); err != nil {
		return
	}
	s.log.Printf("shell stream %d opened", id)
	for {
		browser.SetReadDeadline(time.Now().Add(s.cfg.PingTimeout))
		mt, msg, err := browser.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		typ, _, payload, ok := proto.Decode(msg)
		if !ok {
			continue
		}
		switch typ {
		case proto.FrameData:
			if err := s.sendFrame(proto.FrameData, id, payload); err != nil {
				return
			}
		case proto.FrameWinch:
			if err := s.sendFrame(proto.FrameWinch, id, payload); err != nil {
				return
			}
		case proto.FrameClose:
			return
		}
	}
}

// Addr returns the actual TCP listen address once Run has started.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Run starts the TCP listener and bridges connections until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.ListenAddr, err)
	}
	s.mu.Lock()
	s.ln = ln
	s.addr = ln.Addr().String()
	s.mu.Unlock()
	s.log.Printf("forwarding listener on %s -> client target %s", s.addr, s.cfg.Target)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		tc, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("accept: %w", err)
			}
		}
		go s.handleTCP(tc)
	}
}

func (s *Server) sendFrame(typ byte, id uint32, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	ws := s.ws
	s.mu.Unlock()
	if ws == nil {
		return fmt.Errorf("no client connected")
	}
	ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return ws.WriteMessage(websocket.BinaryMessage, proto.Encode(typ, id, payload))
}

func (s *Server) dropStream(id uint32) {
	s.mu.Lock()
	st, ok := s.streams[id]
	if ok {
		delete(s.streams, id)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	if st.tcp != nil {
		st.tcp.Close()
	}
	if st.shell != nil {
		st.shell.browser.Close()
	}
}

// handleTCP maps one inbound TCP connection to a stream.
func (s *Server) handleTCP(tc net.Conn) {
	s.mu.Lock()
	ws := s.ws
	s.mu.Unlock()
	if ws == nil {
		s.log.Printf("no client connected, refusing %s", tc.RemoteAddr())
		tc.Close()
		return
	}
	id := atomic.AddUint32(&s.nextID, 2) | 1 // odd ids: 1, 3, 5, ...
	s.mu.Lock()
	s.streams[id] = &stream{id: id, tcp: tc}
	s.mu.Unlock()
	defer s.dropStream(id)
	defer s.sendFrame(proto.FrameClose, id, nil)

	if err := s.sendFrame(proto.FrameOpen, id, []byte(s.cfg.Target)); err != nil {
		return
	}
	s.log.Printf("stream %d: %s -> %s", id, tc.RemoteAddr(), s.cfg.Target)
	buf := make([]byte, 32*1024)
	for {
		n, err := tc.Read(buf)
		if n > 0 {
			if werr := s.sendFrame(proto.FrameData, id, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// closeStreamsLocked closes and drops every stream. Caller must hold s.mu.
func (s *Server) closeStreamsLocked() {
	for id, st := range s.streams {
		if st.tcp != nil {
			st.tcp.Close()
		}
		if st.shell != nil {
			st.shell.browser.Close()
		}
		delete(s.streams, id)
	}
}

// attach runs the WebSocket session: it routes DATA/CLOSE frames to streams
// and answers PINGs. It returns when the connection dies.
func (s *Server) attach(c *websocket.Conn) {
	s.mu.Lock()
	s.gen++
	myGen := s.gen
	if s.ws != nil {
		old := s.ws
		s.ws = nil
		old.Close()
	}
	// Take over cleanup of the previous session's streams here. The old
	// session's teardown must not touch this session's map (see the gen
	// check in the defer below), so without this the previous session's
	// conns and handleTCP goroutines would leak.
	s.closeStreamsLocked()
	s.ws = c
	s.streams = make(map[uint32]*stream)
	s.mu.Unlock()
	defer func() {
		if h := s.testBeforeSessionCleanup.Load(); h != nil {
			(*h)()
		}
		s.mu.Lock()
		if s.ws == c {
			s.ws = nil
		}
		// Only tear down streams if no newer session has taken over:
		// an older session's teardown must never walk a newer session's
		// stream map, or streams opened after a reconnect get killed.
		if s.gen == myGen {
			s.closeStreamsLocked()
		}
		s.mu.Unlock()
		c.Close()
	}()

	for {
		c.SetReadDeadline(time.Now().Add(s.cfg.PingTimeout))
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
		case proto.FrameData:
			s.mu.Lock()
			st, found := s.streams[id]
			s.mu.Unlock()
			if !found {
				continue
			}
			if st.tcp != nil {
				st.tcp.SetWriteDeadline(time.Now().Add(30 * time.Second))
				st.tcp.Write(payload)
			} else if st.shell != nil {
				st.shell.sendToBrowser(proto.FrameData, payload)
			}
		case proto.FrameClose:
			s.dropStream(id)
		case proto.FramePing:
			s.writeMu.Lock()
			c.SetWriteDeadline(time.Now().Add(15 * time.Second))
			werr := c.WriteMessage(websocket.BinaryMessage, proto.Encode(proto.FramePong, 0, nil))
			s.writeMu.Unlock()
			if werr != nil {
				return
			}
		}
	}
}
