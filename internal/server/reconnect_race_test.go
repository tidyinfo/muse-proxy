package server

// Regression test for the reconnect race in attach():
// when a new client session takes over, the outgoing session's teardown
// must not close streams opened by the incoming session.
//
// The interleaving is pinned down with the testBeforeSessionCleanup hook
// (an atomic.Pointer[func()] that is nil in production): the outgoing
// teardown parks on it while the test opens a stream on the incoming
// session, then releases it. No sleeps are used to gamble on the
// interleaving timing. Detection of the bug is deterministic too: the
// test stream's conn records Close calls, and the old (buggy) teardown
// unconditionally walks the map, so it always trips the tracker.

import (
	"net"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// closeTracker records whether Close was ever called.
type closeTracker struct {
	net.Conn
	closed atomic.Bool
}

func (c *closeTracker) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestReconnectDoesNotKillNewSessionStreams(t *testing.T) {
	s, err := New(Config{Secret: "s3cret", ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	base := "ws" + strings.TrimPrefix(ts.URL, "http") + "/fwd/s3cret"

	// Session 1 connects.
	c1, _, err := websocket.DefaultDialer.Dial(base, nil)
	if err != nil {
		t.Fatalf("dial session 1: %v", err)
	}
	defer c1.Close()
	waitFor(t, "session 1 attach", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.gen == 1 && s.ws != nil
	})

	// Park session 1's teardown on the hook as soon as it starts.
	entered := make(chan struct{})
	proceed := make(chan struct{})
	var hook func() = func() {
		close(entered)
		<-proceed
	}
	s.testBeforeSessionCleanup.Store(&hook)

	// Session 2 connects and takes over; session 1's teardown begins and
	// parks on the hook.
	c2, _, err := websocket.DefaultDialer.Dial(base, nil)
	if err != nil {
		t.Fatalf("dial session 2: %v", err)
	}
	// Clear the hook before c2 closes: LIFO defers run Store(nil) first,
	// so session 2's own teardown never re-enters the one-shot hook.
	defer s.testBeforeSessionCleanup.Store(nil)
	defer c2.Close()
	waitFor(t, "session 2 takeover", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.gen == 2 && s.ws != nil
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("session 1 teardown never reached the cleanup hook")
	}

	// Open a stream on the incoming (session 2) while the outgoing
	// teardown is parked.
	pc, peer := net.Pipe()
	tracked := &closeTracker{Conn: pc}
	s.mu.Lock()
	s.streams[7] = &stream{id: 7, tcp: tracked}
	s.mu.Unlock()
	defer peer.Close()

	// Release the outgoing teardown. If it walks the new session's map
	// (the bug), it will Close our tracked conn — detect that.
	close(proceed)
	deadline := time.Now().Add(2 * time.Second)
	for !tracked.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if tracked.closed.Load() {
		t.Fatal("stream opened on the incoming session was closed by the outgoing session's teardown")
	}

	s.mu.Lock()
	_, ok := s.streams[7]
	s.mu.Unlock()
	if !ok {
		t.Fatal("stream opened on the incoming session was deleted by the outgoing session's teardown")
	}
	tracked.Close()
}
