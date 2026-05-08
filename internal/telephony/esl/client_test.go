package esl

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"go.uber.org/zap/zaptest"
)

// TestMain installs goleak verification for every test in this package.
// Any goroutine still running after a test exits is a leak — readLoop is
// the only goroutine the package spawns, and it must exit when Close()
// returns.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fakeESLServer simulates a FreeSWITCH ESL listener. Returns the bound
// address and a stop func that closes the listener. The supplied handler
// runs in a goroutine and is responsible for closing the conn it accepts;
// the helper drains additional accepts (rejecting them) so a slow test
// closing late doesn't hang.
//
// The returned stop func waits for the handler goroutine to return so the
// test exits cleanly without leaking goroutines (goleak would catch this
// otherwise).
func fakeESLServer(t *testing.T, handler func(net.Conn)) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var wg sync.WaitGroup
	wg.Go(func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		handler(conn)
	})

	stop := func() {
		_ = ln.Close()
		wg.Wait()
	}
	return ln.Addr().String(), stop
}

// readUntilDoubleNL reads from c until \n\n is observed (the ESL frame
// terminator) or limit bytes have been consumed.
func readUntilDoubleNL(c net.Conn, limit int) (string, error) {
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 64)
	for len(buf) < limit {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := c.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if strings.Contains(string(buf), "\n\n") {
				return string(buf), nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || strings.Contains(err.Error(), "use of closed") {
				return string(buf), nil
			}
			return string(buf), err
		}
	}
	return string(buf), nil
}

// authSuccessHandler is the canonical "happy-path auth" responder. Tests
// that don't need to assert the dialled command's exact bytes use this.
func authSuccessHandler(c net.Conn) {
	_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
	_, _ = readUntilDoubleNL(c, 256)
	_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))
	// Block until the client disconnects so readLoop has something to
	// read against — without this, the conn would close from our side
	// and readLoop's parseFrame would return io.EOF immediately, racing
	// the test's own Close() call.
	tmp := make([]byte, 64)
	for {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(tmp); err != nil {
			return
		}
	}
}

func TestClient_AuthSuccess(t *testing.T) {
	t.Parallel()
	addr, stop := fakeESLServer(t, authSuccessHandler)
	defer stop()

	cli, err := Dial(context.Background(), Config{
		Addr:     addr,
		Password: "ClueCon",
		Logger:   zaptest.NewLogger(t),
	})
	require.NoError(t, err)
	require.True(t, cli.Connected())
	require.NoError(t, cli.Close())
}

func TestClient_AuthSendsCorrectCredential(t *testing.T) {
	t.Parallel()
	var got string
	var gotMu sync.Mutex
	addr, stop := fakeESLServer(t, func(c net.Conn) {
		_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
		s, _ := readUntilDoubleNL(c, 256)
		gotMu.Lock()
		got = s
		gotMu.Unlock()
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))
		tmp := make([]byte, 64)
		for {
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := c.Read(tmp); err != nil {
				return
			}
		}
	})
	defer stop()

	cli, err := Dial(context.Background(), Config{Addr: addr, Password: "ClueCon", Logger: zaptest.NewLogger(t)})
	require.NoError(t, err)
	defer func() { _ = cli.Close() }()

	gotMu.Lock()
	defer gotMu.Unlock()
	require.Contains(t, got, "auth ClueCon")
}

func TestClient_AuthFailure(t *testing.T) {
	t.Parallel()
	addr, stop := fakeESLServer(t, func(c net.Conn) {
		_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
		_, _ = readUntilDoubleNL(c, 256)
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: -ERR invalid\n\n"))
	})
	defer stop()

	_, err := Dial(context.Background(), Config{Addr: addr, Password: "wrong", Logger: zaptest.NewLogger(t)})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrAuthFailed)
}

func TestClient_AuthEmptyReplyTextRejected(t *testing.T) {
	t.Parallel()
	addr, stop := fakeESLServer(t, func(c net.Conn) {
		_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
		_, _ = readUntilDoubleNL(c, 256)
		// Reply with no Reply-Text header at all.
		_, _ = c.Write([]byte("Content-Type: command/reply\n\n"))
	})
	defer stop()

	_, err := Dial(context.Background(), Config{Addr: addr, Password: "x", Logger: zaptest.NewLogger(t)})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrAuthFailed)
}

func TestClient_AuthWrongFirstFrameType(t *testing.T) {
	t.Parallel()
	addr, stop := fakeESLServer(t, func(c net.Conn) {
		// Server sends something other than auth/request.
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK\n\n"))
	})
	defer stop()

	_, err := Dial(context.Background(), Config{Addr: addr, Password: "x", Logger: zaptest.NewLogger(t)})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAuthFailed)
	require.Contains(t, err.Error(), "expected auth/request")
}

func TestClient_ConnectTimeout(t *testing.T) {
	t.Parallel()
	_, err := Dial(context.Background(), Config{
		Addr:           "127.0.0.1:1", // unreachable port
		Password:       "x",
		ConnectTimeout: 100 * time.Millisecond,
		Logger:         zaptest.NewLogger(t),
	})
	require.Error(t, err)
}

func TestClient_DialRequiresAddr(t *testing.T) {
	t.Parallel()
	_, err := Dial(context.Background(), Config{Password: "x"})
	require.Error(t, err)
}

func TestClient_CloseIdempotent(t *testing.T) {
	t.Parallel()
	addr, stop := fakeESLServer(t, authSuccessHandler)
	defer stop()

	cli, err := Dial(context.Background(), Config{Addr: addr, Password: "x", Logger: zaptest.NewLogger(t)})
	require.NoError(t, err)

	require.NoError(t, cli.Close())
	require.NoError(t, cli.Close()) // idempotent — second call is a no-op
	require.False(t, cli.Connected())
}

// TestClient_CloseAfterDisconnectNoticeWaitsForReadLoop proves
// CRITICAL-2: when dispatch's disconnect-notice path has flipped
// closed=true but readLoop is still draining (a race window between
// dispatch returning and the for-loop's next parseFrame observing
// EOF), Close() MUST block on readLoopDone before returning. Without
// the fix the CAS-fail branch returns nil immediately and a caller
// that synchronises on Close() can leak a still-running readLoop.
//
// The package-level goleak.VerifyTestMain in TestMain catches a
// regression here as a leaked readLoop goroutine on package exit; the
// test additionally asserts Close() does not return WHILE the
// readLoop goroutine is still parked.
func TestClient_CloseAfterDisconnectNoticeWaitsForReadLoop(t *testing.T) {
	t.Parallel()

	// The fake server holds the conn open after the disconnect-notice
	// instead of closing it. This widens the race window: readLoop has
	// dispatched the disconnect-notice (and flipped closed=true) but the
	// next parseFrame is still pending. Close()'s call to c.conn.Close()
	// is what kicks readLoop out of parseFrame in this scenario.
	//
	// Pre-fix: Close() sees CAS-fail (closed=true was set by dispatch),
	// returns nil immediately — readLoop is still parked in parseFrame
	// because no one closed conn. Goleak fires on package exit.
	//
	// Post-fix: Close() routes through readLoopDone wait, calling
	// conn.Close on the way; readLoop unblocks, exits, signals done.
	disconnectGate := make(chan struct{})
	holdOpen := make(chan struct{})

	addr, stop := fakeESLServer(t, func(c net.Conn) {
		_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
		_, _ = readUntilDoubleNL(c, 256)
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))
		<-disconnectGate
		body := "Linger: false\nDisconnect-Cause: shutdown\n"
		_, _ = c.Write([]byte("Content-Type: text/disconnect-notice\nContent-Length: " +
			itoa(len(body)) + "\n\n" + body))
		// Hold the conn open so readLoop's next parseFrame stays parked;
		// only close on test teardown.
		<-holdOpen
	})
	defer func() {
		close(holdOpen)
		stop()
	}()

	cli, err := Dial(context.Background(), Config{Addr: addr, Password: "x", Logger: zaptest.NewLogger(t)})
	require.NoError(t, err)

	// Trigger the disconnect-notice. The current-code path is:
	//   readLoop → parseFrame → dispatch(disconnect) → closed.Store(true)
	//                                              → conn.Close()
	// In the new code, conn.Close() in dispatch happens only on CAS
	// success — but either way readLoop will exit shortly. We need to
	// catch the WINDOW BEFORE readLoop's defer fires.
	close(disconnectGate)

	// Spin until the dispatch path has set closed=true. This guarantees
	// the upcoming Close() call hits the CAS-fail branch (the bug
	// location).
	deadline := time.Now().Add(2 * time.Second)
	for !cli.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("dispatch never observed disconnect-notice")
		}
		time.Sleep(time.Millisecond)
	}

	// Now call Close() and assert it returns. With the fix it blocks on
	// readLoopDone → conn.Close() unblocks readLoop → readLoop exits →
	// readLoopDone closes → Close() returns. Without the fix Close()
	// returns nil immediately and readLoop stays parked, leaking on
	// package exit (goleak in TestMain catches that). The 5s budget is
	// generous to avoid flakes; in practice the path completes in <1ms.
	done := make(chan error, 1)
	go func() { done <- cli.Close() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close() blocked indefinitely after disconnect-notice")
	}

	// readLoopDone must be closed by the time Close() returned.
	select {
	case <-cli.readLoopDone:
		// expected
	default:
		t.Fatal("Close() returned but readLoop was still running — goroutine leak")
	}

	require.False(t, cli.Connected())
	// Idempotency: a second Close() must also block on readLoopDone
	// (already closed) and return nil.
	require.NoError(t, cli.Close())
}

func TestClient_DisconnectNoticeClosesEvents(t *testing.T) {
	t.Parallel()
	addr, stop := fakeESLServer(t, func(c net.Conn) {
		_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
		_, _ = readUntilDoubleNL(c, 256)
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))
		// Send a disconnect-notice and let the conn close.
		body := "Linger: false\nDisconnect-Cause: shutdown\n"
		_, _ = c.Write([]byte("Content-Type: text/disconnect-notice\nContent-Length: " +
			itoa(len(body)) + "\n\n" + body))
	})
	defer stop()

	cli, err := Dial(context.Background(), Config{Addr: addr, Password: "x", Logger: zaptest.NewLogger(t)})
	require.NoError(t, err)
	defer func() { _ = cli.Close() }()

	// Events channel must be closed by readLoop within a reasonable
	// budget after disconnect-notice arrives.
	select {
	case _, ok := <-cli.Events():
		require.False(t, ok, "expected events channel closed")
	case <-time.After(2 * time.Second):
		t.Fatal("events channel did not close after disconnect-notice")
	}
	require.False(t, cli.Connected())
}

func TestClient_EventsForwardedToChannel(t *testing.T) {
	t.Parallel()
	addr, stop := fakeESLServer(t, func(c net.Conn) {
		_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
		_, _ = readUntilDoubleNL(c, 256)
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))
		body := "Event-Name: CHANNEL_CREATE\nUnique-ID: u-1\n\n"
		_, _ = c.Write([]byte("Content-Type: text/event-plain\nContent-Length: " +
			itoa(len(body)) + "\n\n" + body))
		// Keep the conn open while the test reads.
		tmp := make([]byte, 64)
		for {
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := c.Read(tmp); err != nil {
				return
			}
		}
	})
	defer stop()

	reg := prometheus.NewRegistry()
	metrics := RegisterMetrics(reg)

	cli, err := Dial(context.Background(), Config{
		Addr: addr, Password: "x",
		Logger:  zaptest.NewLogger(t),
		Metrics: metrics,
	})
	require.NoError(t, err)
	defer func() { _ = cli.Close() }()

	select {
	case ev, ok := <-cli.Events():
		require.True(t, ok)
		require.Equal(t, "CHANNEL_CREATE", ev.Name)
		require.Equal(t, "u-1", ev.UUID)
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive event within 2s")
	}
}

func TestClient_SendCommand_ReturnsReply(t *testing.T) {
	t.Parallel()
	// Vary the command line so the test exercises sendCommand's
	// per-input branching (write/flush/verb-extraction). Higher-layer
	// callers in Task 3 will issue api/bgapi/event commands; the unit
	// test stands in for that diversity here.
	cases := []struct {
		name       string
		line       string
		serverRply string
		wantReply  string
	}{
		{"api-status", "api status", "+OK active", "+OK active"},
		{"event-subscribe", "event plain CHANNEL_CREATE", "+OK", "+OK"},
		{"bgapi-uuid_dump", "bgapi uuid_dump abc-123", "+OK 7f3a-1b2c", "+OK 7f3a-1b2c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			addr, stop := fakeESLServer(t, func(c net.Conn) {
				_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
				_, _ = readUntilDoubleNL(c, 256)
				_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))
				_, _ = readUntilDoubleNL(c, 4096)
				_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: " + tc.serverRply + "\n\n"))
				tmp := make([]byte, 64)
				for {
					_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
					if _, err := c.Read(tmp); err != nil {
						return
					}
				}
			})
			defer stop()

			reg := prometheus.NewRegistry()
			metrics := RegisterMetrics(reg)

			cli, err := Dial(context.Background(), Config{
				Addr: addr, Password: "x",
				Logger: zaptest.NewLogger(t), Metrics: metrics,
			})
			require.NoError(t, err)
			defer func() { _ = cli.Close() }()

			frame, err := cli.sendCommand(context.Background(), tc.line)
			require.NoError(t, err)
			require.Equal(t, tc.wantReply, frame.Header("Reply-Text"))
		})
	}
}

func TestClient_SendCommand_NotConnectedAfterClose(t *testing.T) {
	t.Parallel()
	addr, stop := fakeESLServer(t, authSuccessHandler)
	defer stop()

	cli, err := Dial(context.Background(), Config{Addr: addr, Password: "x", Logger: zaptest.NewLogger(t)})
	require.NoError(t, err)
	require.NoError(t, cli.Close())

	_, err = cli.sendCommand(context.Background(), "api status")
	require.ErrorIs(t, err, ErrNotConnected)
}

func TestClient_SendCommand_ContextCancelReturnsTimeout(t *testing.T) {
	t.Parallel()
	addr, stop := fakeESLServer(t, func(c net.Conn) {
		_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
		_, _ = readUntilDoubleNL(c, 256)
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))
		_, _ = readUntilDoubleNL(c, 4096) // read the test's first command
		// Deliberately don't reply — let the client's ctx expire.
		tmp := make([]byte, 64)
		for {
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := c.Read(tmp); err != nil {
				return
			}
		}
	})
	defer stop()

	cli, err := Dial(context.Background(), Config{Addr: addr, Password: "x", Logger: zaptest.NewLogger(t)})
	require.NoError(t, err)
	defer func() { _ = cli.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = cli.sendCommand(ctx, "api status")
	require.ErrorIs(t, err, ErrTimeout)
}

func TestClient_SendCommand_ConnectionDropMidWait(t *testing.T) {
	t.Parallel()
	addr, stop := fakeESLServer(t, func(c net.Conn) {
		_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
		_, _ = readUntilDoubleNL(c, 256)
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))
		_, _ = readUntilDoubleNL(c, 4096) // read first command, then drop the conn
		// conn closes via defer on handler return → readLoop sees EOF
	})
	defer stop()

	cli, err := Dial(context.Background(), Config{Addr: addr, Password: "x", Logger: zaptest.NewLogger(t)})
	require.NoError(t, err)
	defer func() { _ = cli.Close() }()

	_, err = cli.sendCommand(context.Background(), "api status")
	require.ErrorIs(t, err, ErrNotConnected)
}

func TestClient_UnknownContentTypeIsLogged(t *testing.T) {
	t.Parallel()
	addr, stop := fakeESLServer(t, func(c net.Conn) {
		_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
		_, _ = readUntilDoubleNL(c, 256)
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))
		_, _ = c.Write([]byte("Content-Type: text/something-novel\n\n"))
		tmp := make([]byte, 64)
		for {
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := c.Read(tmp); err != nil {
				return
			}
		}
	})
	defer stop()

	cli, err := Dial(context.Background(), Config{Addr: addr, Password: "x", Logger: zaptest.NewLogger(t)})
	require.NoError(t, err)
	defer func() { _ = cli.Close() }()

	// No assertion possible on the log itself without a sink — but the
	// client must keep functioning afterwards. Issue a command and
	// expect a normal reply.
	// No further server reply will arrive because handler returned, but
	// we just need to verify Connected() stays true while the read pump
	// is alive.
	require.True(t, cli.Connected())
}

func TestClient_MetricsUpdated(t *testing.T) {
	t.Parallel()
	addr, stop := fakeESLServer(t, func(c net.Conn) {
		_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
		_, _ = readUntilDoubleNL(c, 256)
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))
		body := "Event-Name: HEARTBEAT\nUnique-ID: hb\n\n"
		_, _ = c.Write([]byte("Content-Type: text/event-plain\nContent-Length: " +
			itoa(len(body)) + "\n\n" + body))
		tmp := make([]byte, 64)
		for {
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := c.Read(tmp); err != nil {
				return
			}
		}
	})
	defer stop()

	reg := prometheus.NewRegistry()
	metrics := RegisterMetrics(reg)

	cli, err := Dial(context.Background(), Config{
		Addr: addr, Password: "x",
		NodeLabel: "fs-test",
		Logger:    zaptest.NewLogger(t),
		Metrics:   metrics,
	})
	require.NoError(t, err)
	defer func() { _ = cli.Close() }()

	// Wait for the event to arrive (so EventsTotal increments).
	select {
	case <-cli.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("event never arrived")
	}

	// Assert via Gather().
	families, err := reg.Gather()
	require.NoError(t, err)
	var sawConnected, sawEvents bool
	for _, f := range families {
		switch f.GetName() {
		case "esl_connected":
			for _, m := range f.GetMetric() {
				if m.GetGauge().GetValue() == 1 {
					sawConnected = true
				}
			}
		case "esl_events_total":
			for _, m := range f.GetMetric() {
				if m.GetCounter().GetValue() >= 1 {
					sawEvents = true
				}
			}
		}
	}
	require.True(t, sawConnected, "esl_connected gauge did not reach 1")
	require.True(t, sawEvents, "esl_events_total counter did not reach >=1")
}

// TestClient_ConcurrentSendCommandSerializes proves CRITICAL-1 (reply
// stealing): N concurrent sendCommand calls must each receive their
// own reply, not each other's. The fake server tags every reply with
// the unique command counter the client sent, so any cross-talk shows
// up as a mismatch between the goroutine's input and the reply it
// receives.
//
// Pre-fix (writeMu released BEFORE the select-on-replies), a goroutine
// that wins the write race releases the mutex while still pending on
// the shared replies chan; a subsequent caller writes its own command,
// reads the FIRST caller's reply (because Go's runtime hands the chan
// value to whichever goroutine is parked on the receive), and the
// FIRST caller eventually reads the SECOND command's reply. The
// counter-tag mismatch is the visible signature.
//
// Post-fix, cmdMu wraps write+flush+receive-reply as one critical
// section, so at any instant at most one caller is parked on
// c.replies. The 5ms server delay is short enough that all 10 writes
// would pile up on the wire before any reply lands without the lock,
// making cross-talk highly likely; with the lock, commands are
// serialised end-to-end and each caller observes its own reply.
func TestClient_ConcurrentSendCommandSerializes(t *testing.T) {
	t.Parallel()
	const concurrency = 10
	const replyDelay = 5 * time.Millisecond

	addr, stop := fakeESLServer(t, func(c net.Conn) {
		_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
		_, _ = readUntilDoubleNL(c, 256)
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))

		// Read commands one at a time. Each command is "<verb args>\r\n\r\n",
		// so we split incoming bytes on that exact CRLF-CRLF delimiter
		// (NOT plain \n\n — the parser writes CRLF). Multiple commands may
		// arrive in a single Read() (the client batches flushes), so we
		// buffer leftovers across iterations.
		const delim = "\r\n\r\n"
		var buf []byte
		tmp := make([]byte, 256)
		for {
			// Drain any complete commands buffered from previous reads.
			for {
				idx := indexBytes(buf, []byte(delim))
				if idx < 0 {
					break
				}
				cmd := string(buf[:idx])
				buf = buf[idx+len(delim):]
				parts := strings.Fields(cmd)
				if len(parts) == 0 {
					continue
				}
				counter := parts[len(parts)-1]
				time.Sleep(replyDelay)
				reply := "Content-Type: command/reply\nReply-Text: +OK " + counter + "\n\n"
				if _, err := c.Write([]byte(reply)); err != nil {
					return
				}
			}
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, err := c.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if err != nil {
				return
			}
		}
	})
	defer stop()

	cli, err := Dial(context.Background(), Config{Addr: addr, Password: "x", Logger: zaptest.NewLogger(t)})
	require.NoError(t, err)
	defer func() { _ = cli.Close() }()

	type result struct {
		want string
		got  string
		err  error
	}
	results := make(chan result, concurrency)
	var startGate sync.WaitGroup
	startGate.Add(1)

	for i := range concurrency {
		go func(n int) {
			startGate.Wait()
			tag := strconv.Itoa(n)
			frame, err := cli.sendCommand(context.Background(), "api status "+tag)
			got := ""
			if err == nil {
				got = frame.Header("Reply-Text")
			}
			results <- result{want: "+OK " + tag, got: got, err: err}
		}(i)
	}
	startGate.Done()

	collected := make([]result, 0, concurrency)
	for range concurrency {
		select {
		case r := <-results:
			collected = append(collected, r)
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for concurrent sendCommand replies (got %d/%d)",
				len(collected), concurrency)
		}
	}
	for _, r := range collected {
		require.NoError(t, r.err, "sendCommand returned error (want=%s got=%s)", r.want, r.got)
		require.Equal(t, r.want, r.got,
			"reply was stolen by another caller (got=%q, want=%q)", r.got, r.want)
	}
}

// indexBytes is a small helper: index of needle in haystack or -1.
// Used by TestClient_ConcurrentSendCommandSerializes to chunk multi-
// command Read() returns by the ESL "\n\n" frame terminator.
func indexBytes(haystack, needle []byte) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// TestRegisterMetrics_PanicsOnNilRegisterer proves IMPORTANT-5: a nil
// registerer is a wiring error and must panic loudly at boot rather
// than silently produce a *Metrics whose collectors are never
// registered.
func TestRegisterMetrics_PanicsOnNilRegisterer(t *testing.T) {
	t.Parallel()
	require.PanicsWithValue(t,
		"esl.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests",
		func() { _ = RegisterMetrics(nil) },
	)
}

func TestCommandVerb(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		// bgapi-prefixed commands must surface their inner verb so the
		// {command} metric label differentiates originate vs uuid_kill
		// vs uuid_record vs uuid_broadcast (operability — see IMPORTANT-1
		// of the Plan 09 Task 3 review).
		{"bgapi originate {…}", "originate"},
		{"bgapi uuid_kill abc NORMAL_CLEARING", "uuid_kill"},
		{"bgapi uuid_record abc start /path stereo", "uuid_record"},
		{"bgapi uuid_broadcast abc /path aleg", "uuid_broadcast"},
		// api-prefixed commands behave the same way.
		{"api sofia status", "sofia"},
		{"api reloadxml", "reloadxml"},
		{"api xml_flush_cache foo.com", "xml_flush_cache"},
		// Non bgapi/api dispatcher → first token unchanged.
		{"event plain CHANNEL_CREATE", "event"},
		{"singletoken", "singletoken"},
		{"  spaced", "spaced"},
		// Edge: bgapi or api alone (no inner verb) keeps the prefix so
		// the unwrap only fires when a second word actually exists.
		{"bgapi", "bgapi"},
		{"api", "api"},
		// Empty / whitespace-only input → empty string (sendCommand
		// short-circuits before commandVerb on empty lines).
		{"", ""},
		{"\t", ""},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, commandVerb(tc.in), "input=%q", tc.in)
	}
}

// itoa is a small dependency-free strconv.Itoa to keep test imports
// minimal for the trivial body-length splice case.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
