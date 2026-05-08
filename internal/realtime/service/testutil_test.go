package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/service"
)

// fakeWSConn is an in-memory rtapi.WSConn used by every test in this
// package. It models three observable behaviours:
//
//  1. Reads are queued by tests (QueueRead); ReadFrame returns the
//     next queued frame or blocks until one appears.
//  2. Writes are captured for inspection. BlockWrites simulates a
//     slow consumer (writer goroutine wedges); UnblockWrites
//     releases.
//  3. Close records the (code, reason) pair for assertion.
//
// Concurrency: every public method takes the mu; cond is used so
// blocked readers / writers wake up on QueueRead / UnblockWrites.
type fakeWSConn struct {
	mu sync.Mutex
	// cond is signalled on QueueRead, UnblockWrites, and Close so
	// blocked goroutines unwind on lifecycle transitions.
	cond *sync.Cond

	reads      [][]byte
	readErr    error // injected error for the next ReadFrame call
	writes     [][]byte
	writeErr   error
	blocked    bool
	closeCode  rtapi.CloseReason
	closeRsn   string
	closeCount atomic.Int32
	closed     bool
}

func newFakeWSConn() *fakeWSConn {
	f := &fakeWSConn{}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// QueueRead enqueues b as the next frame ReadFrame will return.
func (f *fakeWSConn) QueueRead(b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, b)
	f.cond.Broadcast()
}

// QueueReadErr causes the next ReadFrame call to return err and
// unblock any reader currently waiting.
func (f *fakeWSConn) QueueReadErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readErr = err
	f.cond.Broadcast()
}

// SetWriteErr causes WriteFrame to return err on the next call.
func (f *fakeWSConn) SetWriteErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeErr = err
}

// ReadFrame implements rtapi.WSConn.
func (f *fakeWSConn) ReadFrame(ctx context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.reads) == 0 && f.readErr == nil && !f.closed {
		// Release the lock while waiting for a signal so that
		// ctx-watching goroutines can fire QueueReadErr to
		// unblock us cleanly.
		waitDone := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				f.mu.Lock()
				f.cond.Broadcast()
				f.mu.Unlock()
			case <-waitDone:
			}
		}()
		f.cond.Wait()
		close(waitDone)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if f.readErr != nil {
		err := f.readErr
		f.readErr = nil
		return nil, err
	}
	if f.closed {
		return nil, errFakeClosed
	}
	b := f.reads[0]
	f.reads = f.reads[1:]
	return b, nil
}

var errFakeClosed = errors.New("fakeWSConn: closed")

// WriteFrame implements rtapi.WSConn. When BlockWrites is in effect,
// callers wait until UnblockWrites or the conn is closed.
func (f *fakeWSConn) WriteFrame(ctx context.Context, b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for f.blocked && !f.closed {
		// Mirror the ReadFrame ctx-aware unblock.
		waitDone := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				f.mu.Lock()
				f.cond.Broadcast()
				f.mu.Unlock()
			case <-waitDone:
			}
		}()
		f.cond.Wait()
		close(waitDone)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if f.closed {
		return errFakeClosed
	}
	if f.writeErr != nil {
		err := f.writeErr
		f.writeErr = nil
		return err
	}
	f.writes = append(f.writes, append([]byte(nil), b...))
	return nil
}

// BlockWrites makes subsequent WriteFrame calls block until
// UnblockWrites is called.
func (f *fakeWSConn) BlockWrites() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked = true
}

// UnblockWrites releases all blocked WriteFrame callers.
func (f *fakeWSConn) UnblockWrites() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked = false
	f.cond.Broadcast()
}

// Close records the close arguments. Idempotent at the test layer.
func (f *fakeWSConn) Close(code rtapi.CloseReason, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCount.Add(1)
	if !f.closed {
		f.closed = true
		f.closeCode = code
		f.closeRsn = reason
	}
	f.cond.Broadcast()
	return nil
}

// CloseCode returns the recorded close code.
func (f *fakeWSConn) CloseCode() rtapi.CloseReason {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCode
}

// CloseCallCount returns how many times Close has been invoked. A
// well-behaved Connection close path calls it exactly once.
func (f *fakeWSConn) CloseCallCount() int32 { return f.closeCount.Load() }

// LastWrite returns the most recent payload passed to WriteFrame.
func (f *fakeWSConn) LastWrite() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) == 0 {
		return nil
	}
	return f.writes[len(f.writes)-1]
}

// Writes returns a copy of every captured write.
func (f *fakeWSConn) Writes() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.writes))
	for i, w := range f.writes {
		out[i] = append([]byte(nil), w...)
	}
	return out
}

func (f *fakeWSConn) RemoteAddr() string { return "127.0.0.1:0" }

// stubAuth is a tiny test-only AuthValidator that accepts a single
// hard-coded token. Tests assert behaviour by wiring (validToken,
// claims) and matching the input token.
type stubAuth struct {
	validToken string
	claims     rtapi.Claims
	calls      atomic.Int32
}

// Validate implements service.AuthValidator.
func (s *stubAuth) Validate(_ context.Context, token string) (rtapi.Claims, error) {
	s.calls.Add(1)
	if token != s.validToken {
		return rtapi.Claims{}, service.ErrAuthFailed
	}
	return s.claims, nil
}

// newStubAuth returns a stubAuth that accepts the well-known "valid"
// token. Tests that exercise refresh / mid-session changes mutate
// the returned struct's claims field directly.
func newStubAuth() *stubAuth {
	return &stubAuth{
		validToken: "valid",
		claims: rtapi.Claims{
			UserID:   "user-1",
			TenantID: "tenant-1",
			Roles:    []string{"operator"},
		},
	}
}

// newTestConnection builds a *Connection with sensible test defaults.
func newTestConnection(t *testing.T, cfg service.ConnectionConfig) (*service.Connection, *fakeWSConn) {
	t.Helper()
	if cfg.WriteBufferSize == 0 {
		cfg.WriteBufferSize = 16
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = time.Second
	}
	if cfg.AuthTimeout == 0 {
		cfg.AuthTimeout = time.Second
	}
	if cfg.PingPeriod == 0 {
		cfg.PingPeriod = time.Second
	}
	if cfg.PongTimeout == 0 {
		cfg.PongTimeout = 2 * time.Second
	}
	fake := newFakeWSConn()
	conn := service.NewConnection(fake, cfg)
	return conn, fake
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// newTestHub builds a *Hub with nil metrics and a nop logger,
// pre-wired with the canonical TopicRBAC. The Hub's Shutdown is
// registered as t.Cleanup so a forgotten teardown still leaves no
// dangling registrations across parallel tests.
func newTestHub(t *testing.T) *service.Hub {
	t.Helper()
	hub := service.NewHub(nil, nil, service.NewTopicRBAC())
	t.Cleanup(hub.Shutdown)
	return hub
}
