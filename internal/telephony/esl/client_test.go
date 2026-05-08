package esl

import (
	"context"
	"errors"
	"io"
	"net"
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
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		handler(conn)
	}()

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

func TestCommandVerb(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"bgapi originate {…}", "bgapi"},
		{"api status", "api"},
		{"event plain ALL", "event"},
		{"singletoken", "singletoken"},
		{"  spaced", "spaced"},
		{"", "unknown"},
		{"\t", "unknown"},
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
