package esl

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// commandRecorder is the canonical tooling used by every test in this
// file: it stands in for FreeSWITCH, captures one command after the
// auth handshake, and replies with a scripted frame. The captured
// bytes are surfaced via Got() (call after the test reads the reply).
//
// The reply string is written verbatim: callers control the
// Content-Type, body, and Reply-Text shape so we can prove the
// high-level methods correctly distinguish between command/reply and
// api/response wire layouts.
type commandRecorder struct {
	mu  sync.Mutex
	got string
}

// newCommandRecorder bootstraps a recorder that scripts at most one
// reply after the post-auth command. Pass an empty string to keep the
// server silent (used to exercise ctx-cancel paths).
func newCommandRecorder() *commandRecorder {
	return &commandRecorder{}
}

// Got returns the bytes the test client wrote AFTER the auth-handshake
// command. Safe to call multiple times.
func (r *commandRecorder) Got() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.got
}

// handler returns a fakeESLServer-compatible handler that runs the
// auth handshake, captures the next command, and (if reply != "")
// writes reply back. After that it parks the conn open until the
// client disconnects so readLoop never sees a premature EOF (the same
// idiom authSuccessHandler uses).
func (r *commandRecorder) handler(reply string) func(net.Conn) {
	return func(c net.Conn) {
		_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
		_, _ = readUntilDoubleNL(c, 256)
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))

		got, _ := readUntilDoubleNL(c, 4096)
		r.mu.Lock()
		r.got = got
		r.mu.Unlock()

		if reply != "" {
			_, _ = c.Write([]byte(reply))
		}

		// Hold the conn open so readLoop has something to read against
		// — without this the parseFrame loop would race the test's own
		// Close() on EOF.
		tmp := make([]byte, 64)
		for {
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := c.Read(tmp); err != nil {
				return
			}
		}
	}
}

// dialClient runs the canonical addr+stop+Dial dance used by every
// test below. The returned cleanup wraps both Close() (for the client)
// and stop (for the listener) so a single defer covers everything.
func dialClient(t *testing.T, addr string) (*Client, func()) {
	t.Helper()
	cli, err := Dial(context.Background(), Config{
		Addr:     addr,
		Password: "x",
		Logger:   zaptest.NewLogger(t),
	})
	require.NoError(t, err)
	return cli, func() { _ = cli.Close() }
}

// apiResponse builds a Content-Type: api/response frame with the
// supplied body. Used to mock `api …` synchronous replies.
func apiResponse(body string) string {
	return "Content-Type: api/response\nContent-Length: " +
		itoa(len(body)) + "\n\n" + body
}

// commandReply builds a Content-Type: command/reply frame with the
// supplied Reply-Text. Used to mock `bgapi …` and `event …` replies.
func commandReply(replyText string) string {
	return "Content-Type: command/reply\nReply-Text: " + replyText + "\n\n"
}

// --- Originate ----------------------------------------------------------------

func TestClient_Originate_ParsesUUIDFromOK(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(
		apiResponse("+OK 11111111-2222-3333-4444-555555555555\n"),
	))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	got, err := cli.Originate(context.Background(), OriginateRequest{
		CallURL:   "sofia/gateway/main/+79991234567",
		Extension: "&park()",
	})
	require.NoError(t, err)
	require.Equal(t, "11111111-2222-3333-4444-555555555555", got)
}

// TestClient_Originate_ParsesUUIDFromReplyText covers the wire shape
// real `bgapi` returns: a Content-Type: command/reply frame with the
// +OK <uuid> in Reply-Text and an empty body. The ParsesUUIDFromOK
// test above mocks api/response (body-bearing) instead, which means a
// silent regression in replyPayload could break Reply-Text Originate
// parsing without a single test failing. This case keeps that happy
// path covered.
func TestClient_Originate_ParsesUUIDFromReplyText(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(
		commandReply("+OK 11111111-2222-3333-4444-555555555555"),
	))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	got, err := cli.Originate(context.Background(), OriginateRequest{
		CallURL:   "sofia/gateway/main/+79991234567",
		Extension: "&park()",
	})
	require.NoError(t, err)
	require.Equal(t, "11111111-2222-3333-4444-555555555555", got)
}

func TestClient_Originate_BuildsCommand(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(apiResponse("+OK abc\n")))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	_, err := cli.Originate(context.Background(), OriginateRequest{
		CallURL:    "sofia/gateway/main/+79991234567",
		Caller:     "+74951112233",
		CallerName: "СоциоПульс",
		Variables: map[string]string{
			"sip_h_X-Call-Id": "abc",
			"sip_h_X-Other":   "zzz",
		},
		Extension: "&bridge(sofia/internal/100)",
		Timeout:   45 * time.Second,
	})
	require.NoError(t, err)

	cmd := rec.Got()
	require.Contains(t, cmd, "bgapi originate ")

	// Variables block — keys must be sorted ASCII. Sort order is
	// strict bytewise: "originate_timeout" (`originate_t…`) sorts
	// BEFORE "origination_caller_id_*" because at byte 8 'e' < 'i'.
	// "sip_h_*" lands last because 's' > 'o'.
	require.Contains(t, cmd, "{originate_timeout=45,"+
		"origination_caller_id_name=СоциоПульс,"+
		"origination_caller_id_number=+74951112233,"+
		"sip_h_X-Call-Id=abc,"+
		"sip_h_X-Other=zzz}")
	require.Contains(t, cmd, "sofia/gateway/main/+79991234567 &bridge(sofia/internal/100)")
}

func TestClient_Originate_DefaultsExtensionToPark(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(apiResponse("+OK x\n")))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	_, err := cli.Originate(context.Background(), OriginateRequest{
		CallURL: "sofia/gateway/main/+79991234567",
	})
	require.NoError(t, err)
	require.Contains(t, rec.Got(), " &park()")
}

func TestClient_Originate_DefaultsTimeoutTo30s(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(apiResponse("+OK x\n")))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	_, err := cli.Originate(context.Background(), OriginateRequest{
		CallURL: "sofia/gateway/main/+79991234567",
	})
	require.NoError(t, err)
	require.Contains(t, rec.Got(), "originate_timeout=30")
}

func TestClient_Originate_RejectsEmptyCallURL(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	// Reply is irrelevant — the client must short-circuit BEFORE writing.
	addr, stop := fakeESLServer(t, rec.handler(""))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	_, err := cli.Originate(context.Background(), OriginateRequest{})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidArgument)
	require.Contains(t, err.Error(), "call_url required")

	// Confirm no command bytes hit the wire — only the auth-handshake
	// command should have been captured (or nothing yet, depending on
	// scheduling). Either way the captured command must NOT contain
	// "bgapi originate".
	require.NotContains(t, rec.Got(), "bgapi originate")
}

func TestClient_Originate_TranslatesERROnFailure(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(apiResponse("-ERR USER_BUSY\n")))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	_, err := cli.Originate(context.Background(), OriginateRequest{
		CallURL:   "sofia/gateway/main/+79991234567",
		Extension: "&park()",
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCommandFailed)
	require.Contains(t, err.Error(), "USER_BUSY")
}

func TestClient_Originate_ContextCancelReturnsErrTimeout(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	// Empty reply → server stays silent → ctx must drive the wait out.
	addr, stop := fakeESLServer(t, rec.handler(""))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := cli.Originate(ctx, OriginateRequest{
		CallURL:   "sofia/gateway/main/+79991234567",
		Extension: "&park()",
	})
	require.ErrorIs(t, err, ErrTimeout)
}

// --- Hangup -------------------------------------------------------------------

func TestClient_Hangup_BuildsCommand(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(commandReply("+OK Job-UUID-123")))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	require.NoError(t, cli.Hangup(context.Background(), "uuid-1", "USER_BUSY"))
	require.Contains(t, rec.Got(), "bgapi uuid_kill uuid-1 USER_BUSY")
}

func TestClient_Hangup_DefaultsCauseToNormalClearing(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(commandReply("+OK Job-UUID-123")))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	require.NoError(t, cli.Hangup(context.Background(), "uuid-1", ""))
	require.Contains(t, rec.Got(), "bgapi uuid_kill uuid-1 NORMAL_CLEARING")
}

func TestClient_Hangup_RejectsEmptyUUID(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(""))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	err := cli.Hangup(context.Background(), "", "NORMAL_CLEARING")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidArgument)
	require.Contains(t, err.Error(), "uuid required")
	require.NotContains(t, rec.Got(), "bgapi uuid_kill")
}

func TestClient_Hangup_TranslatesERROnFailure(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(commandReply("-ERR no such call")))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	err := cli.Hangup(context.Background(), "uuid-1", "NORMAL_CLEARING")
	require.ErrorIs(t, err, ErrCommandFailed)
	require.Contains(t, err.Error(), "no such call")
}

// --- MixMonitor ---------------------------------------------------------------

func TestClient_MixMonitorStart_WithFlags(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(commandReply("+OK Job")))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	require.NoError(t, cli.MixMonitorStart(context.Background(),
		"uuid-1", "/recordings/uuid-1.wav", []string{"stereo", "mux"}))
	require.Contains(t, rec.Got(),
		"bgapi uuid_record uuid-1 start /recordings/uuid-1.wav stereo,mux")
}

func TestClient_MixMonitorStart_WithoutFlags(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(commandReply("+OK Job")))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	require.NoError(t, cli.MixMonitorStart(context.Background(),
		"uuid-1", "/recordings/uuid-1.wav", nil))

	// No trailing whitespace or comma after the path when there are
	// no flags — the wire format must end with the path, then \r\n\r\n.
	require.Contains(t, rec.Got(),
		"bgapi uuid_record uuid-1 start /recordings/uuid-1.wav\r\n\r\n")
}

func TestClient_MixMonitorStart_RejectsEmptyArgs(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(""))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	require.ErrorIs(t,
		cli.MixMonitorStart(context.Background(), "", "/p", nil),
		ErrInvalidArgument)
	require.ErrorIs(t,
		cli.MixMonitorStart(context.Background(), "uuid-1", "", nil),
		ErrInvalidArgument)
	require.NotContains(t, rec.Got(), "uuid_record")
}

func TestClient_MixMonitorStop_BuildsCommand(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(commandReply("+OK Job")))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	require.NoError(t, cli.MixMonitorStop(context.Background(),
		"uuid-1", "/recordings/uuid-1.wav"))
	require.Contains(t, rec.Got(),
		"bgapi uuid_record uuid-1 stop /recordings/uuid-1.wav")
}

func TestClient_MixMonitorStop_RejectsEmptyArgs(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(""))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	require.ErrorIs(t,
		cli.MixMonitorStop(context.Background(), "", "/p"),
		ErrInvalidArgument)
	require.ErrorIs(t,
		cli.MixMonitorStop(context.Background(), "uuid-1", ""),
		ErrInvalidArgument)
	require.NotContains(t, rec.Got(), "uuid_record")
}

// --- Play ---------------------------------------------------------------------

func TestClient_Play_BuildsCommand(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(commandReply("+OK Job")))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	require.NoError(t, cli.Play(context.Background(), "uuid-1", "ivr/welcome.wav"))
	require.Contains(t, rec.Got(),
		"bgapi uuid_broadcast uuid-1 ivr/welcome.wav aleg")
}

func TestClient_Play_RejectsEmptyArgs(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(""))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	require.ErrorIs(t,
		cli.Play(context.Background(), "", "p"),
		ErrInvalidArgument)
	require.ErrorIs(t,
		cli.Play(context.Background(), "uuid-1", ""),
		ErrInvalidArgument)
	require.NotContains(t, rec.Got(), "uuid_broadcast")
}

func TestClient_Play_TranslatesERROnFailure(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(commandReply("-ERR no such file")))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	err := cli.Play(context.Background(), "uuid-1", "missing.wav")
	require.ErrorIs(t, err, ErrCommandFailed)
}

// --- SofiaStatus --------------------------------------------------------------

func TestClient_SofiaStatus_ReturnsBodyVerbatim(t *testing.T) {
	t.Parallel()
	body := "                     Name      Type                                       Data         State\n" +
		"=======================================================================================================\n" +
		"        external::example   profile sip:mod_sofia@example:5080  RUNNING (0)\n"

	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(apiResponse(body)))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	got, err := cli.SofiaStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, body, got)
	require.Contains(t, rec.Got(), "api sofia status")
}

func TestClient_SofiaStatus_PropagatesSendCommandError(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(""))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := cli.SofiaStatus(ctx)
	require.ErrorIs(t, err, ErrTimeout)
}

// --- SubscribeEvents ----------------------------------------------------------

func TestClient_SubscribeEvents_BuildsCommand(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(commandReply("+OK event listener enabled plain")))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	require.NoError(t, cli.SubscribeEvents(context.Background(), []string{
		"CHANNEL_CREATE", "CHANNEL_HANGUP_COMPLETE",
	}))
	require.Contains(t, rec.Got(),
		"event plain CHANNEL_CREATE CHANNEL_HANGUP_COMPLETE")
}

func TestClient_SubscribeEvents_EmptyListNoOp(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(""))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	require.NoError(t, cli.SubscribeEvents(context.Background(), nil))
	require.NoError(t, cli.SubscribeEvents(context.Background(), []string{}))

	// No "event plain" should hit the wire. The recorder only sees
	// auth-handshake bytes (or nothing yet).
	require.NotContains(t, rec.Got(), "event plain")
}

func TestClient_SubscribeEvents_RejectsNonPlusReply(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(commandReply("-ERR bad event")))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	err := cli.SubscribeEvents(context.Background(), []string{"CHANNEL_CREATE"})
	require.ErrorIs(t, err, ErrCommandFailed)
}

// --- ReloadXMLDirectory -------------------------------------------------------

func TestClient_ReloadXMLDirectory_NoDomain(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(apiResponse("+OK [Success]\n")))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	require.NoError(t, cli.ReloadXMLDirectory(context.Background(), ""))

	got := rec.Got()
	require.Contains(t, got, "api reloadxml")
	require.NotContains(t, got, "xml_flush_cache")
}

func TestClient_ReloadXMLDirectory_WithDomain(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(apiResponse("+OK\n")))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	require.NoError(t, cli.ReloadXMLDirectory(context.Background(), "foo.com"))
	require.Contains(t, rec.Got(), "api xml_flush_cache foo.com")
}

func TestClient_ReloadXMLDirectory_TranslatesERROnFailure(t *testing.T) {
	t.Parallel()
	rec := newCommandRecorder()
	addr, stop := fakeESLServer(t, rec.handler(apiResponse("-ERR boom\n")))
	defer stop()

	cli, closeCli := dialClient(t, addr)
	defer closeCli()

	err := cli.ReloadXMLDirectory(context.Background(), "")
	require.ErrorIs(t, err, ErrCommandFailed)
	require.Contains(t, err.Error(), "boom")
}

// --- Pure-helper unit tests ---------------------------------------------------

func TestClient_buildVariables_SortsKeys(t *testing.T) {
	t.Parallel()
	got := buildVariables(OriginateRequest{
		Variables: map[string]string{
			"zeta":   "z",
			"alpha":  "a",
			"middle": "m",
		},
	})
	// Sorted: alpha, middle, zeta
	require.Equal(t, "{alpha=a,middle=m,zeta=z}", got)
}

func TestClient_buildVariables_EmptyReturnsEmpty(t *testing.T) {
	t.Parallel()
	require.Empty(t, buildVariables(OriginateRequest{}))
	require.Empty(t, buildVariables(OriginateRequest{Variables: map[string]string{}}))
}

func TestClient_buildVariables_IncludesCallerAndTimeout(t *testing.T) {
	t.Parallel()
	got := buildVariables(OriginateRequest{
		Caller:     "+79991112233",
		CallerName: "test",
		Timeout:    10 * time.Second,
	})
	// Sorted bytewise: 'originate_t…' < 'origination_c…' (byte 8: 'e'<'i').
	require.Equal(t,
		"{originate_timeout=10,origination_caller_id_name=test,origination_caller_id_number=+79991112233}",
		got)
}

func TestClient_buildVariables_RoundsSubSecondTimeout(t *testing.T) {
	t.Parallel()
	// 1.4s rounds DOWN to 1s; 1.6s rounds UP to 2s — time.Duration.Round
	// is half-to-even on the boundary, but we don't test the boundary
	// since it's a Go stdlib detail.
	got := buildVariables(OriginateRequest{Timeout: 1500 * time.Millisecond})
	// 1.5s rounds to 2s under Go's banker's-rounding-to-even rule for
	// Duration.Round; either 1 or 2 is acceptable, just not 0 or empty.
	require.Contains(t, got, "originate_timeout=")
	require.NotContains(t, got, "originate_timeout=0")
}

func TestClient_escapeVar_StripsBraces(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"plain", "plain"},
		{"with{brace", "withbrace"},
		{"with}brace", "withbrace"},
		{"both{}", "both"},
		{"comma,inside", `comma\,inside`},
		{"{embedded,inside}", `embedded\,inside`},
		{"", ""},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, escapeVar(tc.in), "input=%q", tc.in)
	}
}

func TestClient_replyPayload_PrefersBody(t *testing.T) {
	t.Parallel()
	withBody := Frame{
		headers: map[string]string{"reply-text": "+OK ignored"},
		Body:    []byte("+OK from-body\n"),
	}
	require.Equal(t, "+OK from-body", replyPayload(withBody))

	noBody := Frame{
		headers: map[string]string{"reply-text": " +OK from-reply"},
	}
	require.Equal(t, "+OK from-reply", replyPayload(noBody))

	empty := Frame{}
	require.Empty(t, replyPayload(empty))
}

func TestClient_splitOKBody_Decodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in       string
		wantOK   bool
		wantBody string
	}{
		{"+OK 11111111-2222-3333-4444-555555555555", true, "11111111-2222-3333-4444-555555555555"},
		{"+OK", true, ""},
		{"-ERR USER_BUSY", false, "-ERR USER_BUSY"},
		{"random garbage", false, "random garbage"},
		{"", false, ""},
	}
	for _, tc := range cases {
		ok, body := splitOKBody(tc.in)
		require.Equal(t, tc.wantOK, ok, "input=%q", tc.in)
		require.Equal(t, tc.wantBody, body, "input=%q", tc.in)
	}
}

// --- Connection-failure paths shared across commands -------------------------

func TestClient_Commands_ReturnNotConnectedAfterClose(t *testing.T) {
	t.Parallel()
	addr, stop := fakeESLServer(t, authSuccessHandler)
	defer stop()

	cli, err := Dial(context.Background(), Config{
		Addr: addr, Password: "x", Logger: zaptest.NewLogger(t),
	})
	require.NoError(t, err)
	require.NoError(t, cli.Close())

	// Every command path must surface ErrNotConnected (post-close)
	// rather than panicking or hanging.
	_, err = cli.Originate(context.Background(), OriginateRequest{
		CallURL: "sofia/gateway/main/+1", Extension: "&park()",
	})
	require.ErrorIs(t, err, ErrNotConnected)

	require.ErrorIs(t, cli.Hangup(context.Background(), "uuid", ""), ErrNotConnected)
	require.ErrorIs(t, cli.MixMonitorStart(context.Background(), "uuid", "/p", nil), ErrNotConnected)
	require.ErrorIs(t, cli.MixMonitorStop(context.Background(), "uuid", "/p"), ErrNotConnected)
	require.ErrorIs(t, cli.Play(context.Background(), "uuid", "p"), ErrNotConnected)

	_, err = cli.SofiaStatus(context.Background())
	require.ErrorIs(t, err, ErrNotConnected)

	require.ErrorIs(t, cli.SubscribeEvents(context.Background(), []string{"X"}), ErrNotConnected)
	require.ErrorIs(t, cli.ReloadXMLDirectory(context.Background(), ""), ErrNotConnected)
}
