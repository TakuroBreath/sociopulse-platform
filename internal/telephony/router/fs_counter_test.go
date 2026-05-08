package router_test

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/telephony/esl"
	"github.com/sociopulse/platform/internal/telephony/router"
)

// fakeClientLookup is a stub ClientLookup whose Get hook is a function
// field. Tests configure the hook per-case to return either a healthy
// *esl.Client (dialled against a small in-process fake) or an
// esl.ErrNotConnected wrapper, exercising the two ESLFSCounter branches.
type fakeClientLookup struct {
	getFn func(addr string) (*esl.Client, error)
}

func (f *fakeClientLookup) Get(addr string) (*esl.Client, error) {
	return f.getFn(addr)
}

// fakeESLForCounter is a minimal ESL listener used by ESLFSCounter tests.
// It runs the post-auth canned reply for `api show channels count`
// (caller-controlled body) and parks the connection open so esl.Client's
// readLoop has something to wait on. Mirrors the pattern in
// internal/telephony/esl and pool packages — kept inline so the router
// package's tests do not gain a cross-package test-helper dependency.
//
// Returns the bound addr and a stop closure that closes the listener AND
// every accepted connection, then waits for handler goroutines to exit
// (so goleak in TestMain stays clean).
func fakeESLForCounter(t *testing.T, body string) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var (
		mu    sync.Mutex
		conns []net.Conn
		wg    sync.WaitGroup
	)

	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			wg.Go(func() {
				defer func() { _ = conn.Close() }()
				channelsCountHandler(conn, body)
			})
		}
	})

	stop := func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	}
	return ln.Addr().String(), stop
}

// channelsCountHandler runs a single-shot ESL exchange: auth handshake,
// then for every subsequent command line emits an api/response frame
// carrying body. Parks until the conn closes so the client-side readLoop
// observes a clean EOF rather than a deadline violation.
func channelsCountHandler(c net.Conn, body string) {
	_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
	if err := readUntilDoubleNLLocal(c, 256); err != nil {
		return
	}
	_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))

	for {
		if err := readUntilDoubleNLLocal(c, 4096); err != nil {
			return
		}
		_, _ = fmt.Fprintf(c,
			"Content-Type: api/response\nContent-Length: %d\n\n%s",
			len(body), body,
		)
	}
}

// readUntilDoubleNLLocal is a minimal frame-terminator scanner. ESL clients
// use \r\n\r\n; some test paths use \n\n — accept both, mirroring the
// helpers in pool_test.go. Returns nil on a successful read of a complete
// frame and the underlying conn error otherwise; the consumed bytes are
// discarded (the handler doesn't need to inspect them).
func readUntilDoubleNLLocal(c net.Conn, limit int) error {
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 64)
	for len(buf) < limit {
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		n, err := c.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			s := string(buf)
			if strings.Contains(s, "\r\n\r\n") || strings.Contains(s, "\n\n") {
				return nil
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// dialESLForCounter wires a real *esl.Client at the bound address; the
// ESLFSCounter then issues `api show channels count` against this client.
// Cleanup closes the client; goleak ensures the readLoop exits.
func dialESLForCounter(t *testing.T, addr string) *esl.Client {
	t.Helper()
	cli, err := esl.Dial(context.Background(), esl.Config{
		Addr:     addr,
		Password: "x",
		Logger:   zaptest.NewLogger(t),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// --- Constructor --------------------------------------------------------------

func TestNewESLFSCounter_RejectsNilLookup(t *testing.T) {
	t.Parallel()
	_, err := router.NewESLFSCounter(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ClientLookup")
}

func TestNewESLFSCounter_AcceptsLookup(t *testing.T) {
	t.Parallel()
	c, err := router.NewESLFSCounter(&fakeClientLookup{
		getFn: func(string) (*esl.Client, error) { return nil, esl.ErrNotConnected },
	})
	require.NoError(t, err)
	require.NotNil(t, c)
}

// --- ActiveChannels: pool-lookup error ---------------------------------------

// TestESLFSCounter_PropagatesPoolLookupError covers the path where the pool
// reports the node disconnected (ErrNotConnected) — ActiveChannels wraps
// the error preserving errors.Is identity so the reconciler can log + skip.
func TestESLFSCounter_PropagatesPoolLookupError(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("pool.Get fs-1: %w", esl.ErrNotConnected)
	c, err := router.NewESLFSCounter(&fakeClientLookup{
		getFn: func(string) (*esl.Client, error) { return nil, wrapped },
	})
	require.NoError(t, err)

	_, err = c.ActiveChannels(context.Background(), "fs-1")
	require.Error(t, err)
	require.ErrorIs(t, err, esl.ErrNotConnected,
		"pool error must be wrapped with %%w so reconciler can errors.Is")
	require.Contains(t, err.Error(), "fs-1")
}

// --- ActiveChannels: happy path ----------------------------------------------

// TestESLFSCounter_ReturnsCount asserts the FS-shape body "5 total." is
// parsed as 5. Wire format details (api show channels count, body
// extraction) are owned by *esl.Client.ChannelsCount; this test exists to
// prove the ESLFSCounter glue actually invokes that path.
func TestESLFSCounter_ReturnsCount(t *testing.T) {
	t.Parallel()
	addr, stop := fakeESLForCounter(t, "5 total.\n")
	defer stop()

	cli := dialESLForCounter(t, addr)

	c, err := router.NewESLFSCounter(&fakeClientLookup{
		getFn: func(string) (*esl.Client, error) { return cli, nil },
	})
	require.NoError(t, err)

	got, err := c.ActiveChannels(context.Background(), "fs-1")
	require.NoError(t, err)
	require.Equal(t, 5, got)
}

// TestESLFSCounter_PropagatesESLError surfaces ChannelsCount errors with
// "show channels count" context so a non-numeric FS body is logged with a
// useful breadcrumb.
func TestESLFSCounter_PropagatesESLError(t *testing.T) {
	t.Parallel()
	addr, stop := fakeESLForCounter(t, "garbage in body\n")
	defer stop()

	cli := dialESLForCounter(t, addr)

	c, err := router.NewESLFSCounter(&fakeClientLookup{
		getFn: func(string) (*esl.Client, error) { return cli, nil },
	})
	require.NoError(t, err)

	_, err = c.ActiveChannels(context.Background(), "fs-1")
	require.ErrorIs(t, err, esl.ErrCommandFailed,
		"non-numeric body must surface as ErrCommandFailed (wrapped)")
	require.Contains(t, err.Error(), "show channels count")
}
