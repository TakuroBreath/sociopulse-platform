package esl

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Default channel sizes & timeouts. Exported via Config for callers that
// want different values.
const (
	// defaultEventBuffer is the events channel capacity. 1024 is enough
	// to absorb a 60-channel call-burst (one CHANNEL_CREATE +
	// CHANNEL_ANSWER + bridge + hangup_complete = 4 events per call,
	// 60 calls × 4 = 240 events) without blocking the readLoop, even
	// when the consumer is briefly slow.
	defaultEventBuffer = 1024

	// defaultReplyBuffer is the replies channel capacity. Inflight
	// commands are serialised by Client.mu, so capacity 16 only matters
	// when the readLoop sees replies the caller has already given up on
	// (ctx cancelled mid-flight). Drop-on-full keeps readLoop responsive.
	defaultReplyBuffer = 16

	// defaultConnectTimeout caps DialContext on the initial TCP connect.
	defaultConnectTimeout = 10 * time.Second

	// defaultReadTimeout is the inactivity deadline applied to readLoop.
	// We add 30s slack on top so that an FS HEARTBEAT (default 20s
	// interval) doesn't trip a false disconnect.
	defaultReadTimeout = 60 * time.Second
)

// Config configures Dial. All fields are optional; defaults() fills the
// blanks.
type Config struct {
	// Addr is the FreeSWITCH ESL endpoint, e.g. "127.0.0.1:8021".
	// Required — Dial returns an error when empty.
	Addr string

	// Password is the ESL password (Cleartext on the wire — TCP only,
	// no TLS in v1). Sent as `auth <password>` after we receive
	// auth/request.
	Password string

	// ConnectTimeout caps the TCP DialContext before we give up. Zero
	// uses defaultConnectTimeout.
	ConnectTimeout time.Duration

	// ReadTimeout is the per-frame inactivity deadline applied during
	// readLoop. Zero uses defaultReadTimeout. Set negative to disable
	// (not recommended outside tests).
	ReadTimeout time.Duration

	// Logger is the zap logger used for warn/info/debug messages from
	// readLoop. Zero uses zap.NewNop() — production callers should pass
	// a real logger named for the node.
	Logger *zap.Logger

	// Metrics receives gauge / counter / histogram updates. Zero leaves
	// metrics off entirely (production callers should pass the result
	// of RegisterMetrics).
	Metrics *Metrics

	// NodeLabel is the label value for "node" on every metric this
	// client emits. Defaults to Addr.
	NodeLabel string
}

// defaults populates zero-valued fields with sane fallbacks. Mutates the
// receiver — must be called before any field is read off cfg.
func (c *Config) defaults() {
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = defaultConnectTimeout
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = defaultReadTimeout
	}
	if c.Logger == nil {
		c.Logger = zap.NewNop()
	}
	if c.NodeLabel == "" {
		c.NodeLabel = c.Addr
	}
}

// Client is a single-connection ESL client to one FreeSWITCH node. It
// owns one readLoop goroutine that demultiplexes incoming frames into
// the events and replies channels.
//
// Concurrency:
//   - Close() is idempotent and goroutine-safe via the closed atomic.Bool.
//   - sendCommand serialises writes through mu; only one inflight command
//     at a time per Client.
//   - readLoop is the SOLE writer to events and replies. It also
//     exclusively closes events on exit. No other goroutine touches
//     these channels' close state.
//   - connected.Load() may transiently return true after close has been
//     observed; callers needing strict guarantees should compose with
//     <-Done() (not exposed here — Task 4 may add).
type Client struct {
	cfg       Config
	conn      net.Conn
	reader    *bufio.Reader
	writer    *bufio.Writer
	writeMu   sync.Mutex // serialises sendCommand writes
	connected atomic.Bool
	closed    atomic.Bool

	events  chan Event
	replies chan Frame

	// readLoopDone signals that the readLoop goroutine has fully exited.
	// Close() blocks on this so callers can rely on goroutine quiescence
	// after Close() returns.
	readLoopDone chan struct{}
}

// Dial opens a TCP connection to cfg.Addr, performs the ESL auth
// handshake, and returns a *Client ready to issue commands and stream
// events. The supplied ctx bounds the dial-and-handshake duration; once
// Dial returns successfully the ctx is no longer consulted.
//
// Returns ErrAuthFailed when the server rejects our password.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("esl.Dial: Addr required")
	}
	cfg.defaults()

	dialCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	d := net.Dialer{}
	conn, err := d.DialContext(dialCtx, "tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.Addr, err)
	}

	cli := &Client{
		cfg:          cfg,
		conn:         conn,
		reader:       bufio.NewReaderSize(conn, 64*1024),
		writer:       bufio.NewWriterSize(conn, 8*1024),
		events:       make(chan Event, defaultEventBuffer),
		replies:      make(chan Frame, defaultReplyBuffer),
		readLoopDone: make(chan struct{}),
	}

	// Apply the dial deadline to the auth-handshake reads/writes too —
	// otherwise a stalled FS would leave us blocked on an unbounded
	// parseFrame call before readLoop even starts.
	if dl, ok := dialCtx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	if err := cli.authenticate(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	// Clear handshake deadline; readLoop manages its own per-frame
	// deadline. Ignore the error: a closed conn would have failed the
	// handshake first.
	_ = conn.SetDeadline(time.Time{})

	cli.connected.Store(true)
	if cli.cfg.Metrics != nil {
		cli.cfg.Metrics.Connected.WithLabelValues(cli.cfg.NodeLabel).Set(1)
	}

	go cli.readLoop()
	return cli, nil
}

// authenticate runs the auth/request → `auth <password>` → command/reply
// exchange. Called exactly once, by Dial, before readLoop starts.
func (c *Client) authenticate() error {
	frame, err := parseFrame(c.reader)
	if err != nil {
		return fmt.Errorf("read auth/request: %w", err)
	}
	if frame.ContentType() != "auth/request" {
		return fmt.Errorf("expected auth/request, got %q", frame.ContentType())
	}

	if _, err := fmt.Fprintf(c.writer, "auth %s\r\n\r\n", c.cfg.Password); err != nil {
		return fmt.Errorf("write auth: %w", err)
	}
	if err := c.writer.Flush(); err != nil {
		return fmt.Errorf("flush auth: %w", err)
	}

	resp, err := parseFrame(c.reader)
	if err != nil {
		return fmt.Errorf("read auth reply: %w", err)
	}

	reply := strings.TrimLeft(resp.Header("Reply-Text"), " \t")
	if !strings.HasPrefix(reply, "+") {
		return fmt.Errorf("%w: %s", ErrAuthFailed, reply)
	}
	return nil
}

// Connected reports whether the client believes it has a healthy ESL
// connection. Returns false after Close() and after readLoop observes
// EOF / disconnect-notice.
func (c *Client) Connected() bool {
	return c.connected.Load() && !c.closed.Load()
}

// Events returns a receive-only channel of decoded events. The channel
// is closed exactly once, by readLoop, when the client tears down (Close
// called, EOF observed, or text/disconnect-notice received). Callers
// MUST drain or stop ranging on close.
func (c *Client) Events() <-chan Event {
	return c.events
}

// Close shuts the TCP connection and waits for readLoop to drain.
// Idempotent — every call after the first returns nil. Safe to call from
// any goroutine.
func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.connected.Store(false)
	if c.cfg.Metrics != nil {
		c.cfg.Metrics.Connected.WithLabelValues(c.cfg.NodeLabel).Set(0)
	}
	// Closing the underlying conn unblocks readLoop's parseFrame.
	err := c.conn.Close()
	<-c.readLoopDone
	return err
}

// readLoop is the single reader of c.reader. It runs in its own
// goroutine, started by Dial. Exits on the first parseFrame error or on
// receiving text/disconnect-notice, after which it closes the events
// channel and signals readLoopDone.
//
// readLoop is the SOLE owner of `events`'s close state and the SOLE
// writer to both events and replies (single-writer rule).
func (c *Client) readLoop() {
	defer func() {
		c.connected.Store(false)
		if c.cfg.Metrics != nil {
			c.cfg.Metrics.Connected.WithLabelValues(c.cfg.NodeLabel).Set(0)
		}
		close(c.events)
		close(c.readLoopDone)
	}()

	for {
		if c.cfg.ReadTimeout > 0 {
			// Conservative slack on top of ReadTimeout: FS HEARTBEAT
			// every 20s keeps the socket alive; a true network hang
			// will trip the deadline + 30s slack.
			_ = c.conn.SetReadDeadline(time.Now().Add(c.cfg.ReadTimeout + 30*time.Second))
		}

		frame, err := parseFrame(c.reader)
		if err != nil {
			if !c.closed.Load() {
				c.cfg.Logger.Warn("esl readLoop exit",
					zap.String("addr", c.cfg.Addr),
					zap.Error(err),
				)
			}
			return
		}

		c.dispatch(frame)
	}
}

// dispatch routes a single frame to the right destination. Pulled out
// of readLoop to keep the loop short and to make the per-content-type
// behaviour exhaustively testable.
func (c *Client) dispatch(frame Frame) {
	switch frame.ContentType() {
	case "text/event-plain", "text/event-json":
		ev, err := frame.AsEvent()
		if err != nil {
			c.cfg.Logger.Warn("esl event decode",
				zap.String("addr", c.cfg.Addr),
				zap.Error(err),
			)
			return
		}
		if c.cfg.Metrics != nil {
			c.cfg.Metrics.EventsTotal.WithLabelValues(c.cfg.NodeLabel, ev.Name).Inc()
		}
		select {
		case c.events <- ev:
		default:
			// Drop on backpressure rather than block readLoop. Lost
			// events are a known degradation mode — see references doc
			// gotcha #9 (FS does not buffer events for disconnected
			// clients anyway).
			c.cfg.Logger.Warn("esl event dropped (channel full)",
				zap.String("addr", c.cfg.Addr),
				zap.String("event", ev.Name),
			)
		}

	case "command/reply", "api/response":
		select {
		case c.replies <- frame:
		default:
			// Caller cancelled mid-flight or the reply arrived after
			// we'd given up on it. Either way, dropping here is
			// preferable to deadlocking readLoop.
			c.cfg.Logger.Warn("esl reply dropped (channel full)",
				zap.String("addr", c.cfg.Addr),
			)
		}

	case "text/disconnect-notice":
		c.cfg.Logger.Info("esl disconnect notice",
			zap.String("addr", c.cfg.Addr),
		)
		// Returning nothing here would just continue the loop; we want
		// to mirror FS's intent and tear down. Setting closed → true
		// makes the next parseFrame loop iteration's read fail (or the
		// already-pending Close() complete cleanly).
		c.closed.Store(true)
		_ = c.conn.Close()

	default:
		c.cfg.Logger.Warn("esl unknown content-type",
			zap.String("addr", c.cfg.Addr),
			zap.String("content_type", frame.ContentType()),
		)
	}
}

// sendCommand writes line + the ESL terminator to the wire and waits for
// the next reply (command/reply or api/response). Concurrent
// sendCommand calls serialise on c.writeMu — one inflight command per
// Client, matching the protocol's reply-by-arrival semantics.
//
// The supplied ctx bounds the wait for a reply. Cancellation surfaces as
// ErrTimeout. Disconnection mid-wait surfaces as ErrNotConnected.
//
//nolint:unused // wired by Task 3 (high-level commands: Originate, Hangup, MixMonitor, Play, SofiaStatus).
func (c *Client) sendCommand(ctx context.Context, line string) (Frame, error) {
	if !c.Connected() {
		return Frame{}, ErrNotConnected
	}

	verb := commandVerb(line)
	start := time.Now()

	c.writeMu.Lock()
	if _, err := fmt.Fprintf(c.writer, "%s\r\n\r\n", line); err != nil {
		c.writeMu.Unlock()
		c.recordCommand(verb, "err", time.Since(start))
		return Frame{}, fmt.Errorf("write: %w", err)
	}
	if err := c.writer.Flush(); err != nil {
		c.writeMu.Unlock()
		c.recordCommand(verb, "err", time.Since(start))
		return Frame{}, fmt.Errorf("flush: %w", err)
	}
	c.writeMu.Unlock()

	select {
	case f, ok := <-c.replies:
		if !ok {
			c.recordCommand(verb, "err", time.Since(start))
			return Frame{}, ErrNotConnected
		}
		c.recordCommand(verb, "ok", time.Since(start))
		return f, nil
	case <-ctx.Done():
		c.recordCommand(verb, "timeout", time.Since(start))
		return Frame{}, ErrTimeout
	case <-c.readLoopDone:
		c.recordCommand(verb, "err", time.Since(start))
		return Frame{}, ErrNotConnected
	}
}

// recordCommand updates Metrics.CommandsTotal + CommandDuration when
// metrics are wired. Pulled out so sendCommand stays linear.
func (c *Client) recordCommand(verb, result string, dur time.Duration) {
	if c.cfg.Metrics == nil {
		return
	}
	c.cfg.Metrics.CommandsTotal.WithLabelValues(c.cfg.NodeLabel, verb, result).Inc()
	c.cfg.Metrics.CommandDuration.WithLabelValues(c.cfg.NodeLabel, verb).Observe(dur.Seconds())
}

// commandVerb extracts the first whitespace-delimited token from line —
// e.g. "bgapi originate {…}…" → "bgapi". Used as the {command} label on
// metrics so cardinality stays bounded (the originate URL would be a
// per-call unique value otherwise).
func commandVerb(line string) string {
	line = strings.TrimSpace(line)
	if idx := strings.IndexAny(line, " \t"); idx > 0 {
		return line[:idx]
	}
	if line == "" {
		return "unknown"
	}
	return line
}
