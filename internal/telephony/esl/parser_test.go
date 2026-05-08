package esl

import (
	"bufio"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// makeReader is a tiny convenience wrapping a string in bufio.Reader.
func makeReader(s string) *bufio.Reader {
	return bufio.NewReader(strings.NewReader(s))
}

// withBody assembles a frame with an explicit Content-Length so test cases
// stay honest about what they're feeding the parser.
func withBody(headers, body string) string {
	return headers + "Content-Length: " + strconv.Itoa(len(body)) + "\n\n" + body
}

func TestParseFrame_AuthRequest(t *testing.T) {
	t.Parallel()
	frame, err := parseFrame(makeReader("Content-Type: auth/request\n\n"))
	require.NoError(t, err)
	require.Equal(t, "auth/request", frame.Header("Content-Type"))
	require.Equal(t, "auth/request", frame.ContentType())
	require.Empty(t, frame.Body)
}

func TestParseFrame_HeaderLookupCaseInsensitive(t *testing.T) {
	t.Parallel()
	frame, err := parseFrame(makeReader("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))
	require.NoError(t, err)
	// Mixed-case lookups all hit the same value because the parser
	// lowercases keys at insert time.
	require.Equal(t, "+OK accepted", frame.Header("Reply-Text"))
	require.Equal(t, "+OK accepted", frame.Header("reply-text"))
	require.Equal(t, "+OK accepted", frame.Header("REPLY-TEXT"))
	// Unknown headers return empty string (not an error).
	require.Empty(t, frame.Header("nonexistent"))
}

func TestParseFrame_CommandReplyOK(t *testing.T) {
	t.Parallel()
	frame, err := parseFrame(makeReader("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))
	require.NoError(t, err)
	require.Equal(t, "command/reply", frame.ContentType())
	require.Equal(t, "+OK accepted", frame.Header("Reply-Text"))
}

func TestParseFrame_CommandReplyError(t *testing.T) {
	t.Parallel()
	frame, err := parseFrame(makeReader("Content-Type: command/reply\nReply-Text: -ERR invalid password\n\n"))
	require.NoError(t, err)
	require.Equal(t, "-ERR invalid password", frame.Header("Reply-Text"))
}

func TestParseFrame_CommandReplyJobUUID(t *testing.T) {
	t.Parallel()
	// Some FS builds return Job-UUID via Reply-Text after `bgapi` originates.
	frame, err := parseFrame(makeReader("Content-Type: command/reply\nReply-Text: +OK 7f3a-1b2c\nJob-UUID: 7f3a-1b2c\n\n"))
	require.NoError(t, err)
	require.Equal(t, "+OK 7f3a-1b2c", frame.Header("Reply-Text"))
	require.Equal(t, "7f3a-1b2c", frame.Header("Job-UUID"))
}

func TestParseFrame_ApiResponseWithBody(t *testing.T) {
	t.Parallel()
	body := "BODY-DATA-HERE\n"
	raw := withBody("Content-Type: api/response\n", body)
	frame, err := parseFrame(makeReader(raw))
	require.NoError(t, err)
	require.Equal(t, "api/response", frame.ContentType())
	require.Equal(t, body, string(frame.Body))
}

func TestParseFrame_EventPlain(t *testing.T) {
	t.Parallel()
	body := "Event-Name: CHANNEL_CREATE\n" +
		"Unique-ID: abc-123\n" +
		"Caller-Caller-ID-Number: %2B79991234567\n" + // URL-encoded "+79991234567"
		"Variable-sip_to_uri: sip%3Auser%40host%3A5060\n\n" // "sip:user@host:5060"
	raw := withBody("Content-Type: text/event-plain\n", body)

	frame, err := parseFrame(makeReader(raw))
	require.NoError(t, err)
	require.Equal(t, "text/event-plain", frame.ContentType())

	ev, err := frame.AsEvent()
	require.NoError(t, err)
	require.Equal(t, "CHANNEL_CREATE", ev.Name)
	require.Equal(t, "abc-123", ev.UUID)
	require.Equal(t, "+79991234567", ev.Header("Caller-Caller-ID-Number"))
	require.Equal(t, "sip:user@host:5060", ev.Header("Variable-sip_to_uri"))
	// Case-insensitive event header lookup, same contract as Frame.
	require.Equal(t, "CHANNEL_CREATE", ev.Header("event-name"))
}

func TestParseFrame_EventJsonStillRoutesAsEvent(t *testing.T) {
	t.Parallel()
	// AsEvent() distinguishes "event" frames by Content-Type substring.
	// We don't actually parse JSON bodies in this implementation — Plan 09
	// uses event-plain — but the content-type acceptance prevents
	// AsEvent from refusing a future event-json subscription.
	body := "Event-Name: HEARTBEAT\nUnique-ID: \n\n"
	raw := withBody("Content-Type: text/event-json\n", body)
	frame, err := parseFrame(makeReader(raw))
	require.NoError(t, err)
	_, err = frame.AsEvent()
	require.NoError(t, err)
}

func TestParseFrame_DisconnectNotice(t *testing.T) {
	t.Parallel()
	// FS sends text/disconnect-notice with a small body explaining why.
	body := "Linger: false\nDisconnect-Cause: shutdown\n"
	raw := withBody("Content-Type: text/disconnect-notice\n", body)
	frame, err := parseFrame(makeReader(raw))
	require.NoError(t, err)
	require.Equal(t, "text/disconnect-notice", frame.ContentType())
	require.Equal(t, len(body), len(frame.Body))
}

func TestParseFrame_MalformedHeaderRejected(t *testing.T) {
	t.Parallel()
	// No colon — should produce ErrInvalidFrame.
	_, err := parseFrame(makeReader("ContentType auth/request\n\n"))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidFrame)
}

func TestParseFrame_LeadingColonRejected(t *testing.T) {
	t.Parallel()
	// Header line starting with ":" → no name → idx <= 0.
	_, err := parseFrame(makeReader(":empty-name\n\n"))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidFrame)
}

func TestParseFrame_NonNumericContentLengthRejected(t *testing.T) {
	t.Parallel()
	_, err := parseFrame(makeReader("Content-Type: api/response\nContent-Length: NaN\n\n"))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidFrame)
}

func TestParseFrame_NegativeContentLengthRejected(t *testing.T) {
	t.Parallel()
	_, err := parseFrame(makeReader("Content-Type: api/response\nContent-Length: -1\n\n"))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidFrame)
}

func TestParseFrame_BodyTruncated(t *testing.T) {
	t.Parallel()
	// Headers promise 100 bytes but supply 4. io.ReadFull → io.ErrUnexpectedEOF.
	raw := "Content-Type: api/response\nContent-Length: 100\n\nABCD"
	_, err := parseFrame(makeReader(raw))
	require.Error(t, err)
	// Parser wraps the underlying error — confirm we surface ReadFull's signal.
	// io.ReadFull returns io.ErrUnexpectedEOF when it gets some-but-not-all bytes;
	// either io.EOF or io.ErrUnexpectedEOF is acceptable depending on buffering.
	if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF-family error, got %v", err)
	}
}

func TestParseFrame_EOFAtStart(t *testing.T) {
	t.Parallel()
	// Empty stream → io.EOF surfaces verbatim (caller decides retry policy).
	_, err := parseFrame(makeReader(""))
	require.Error(t, err)
	require.ErrorIs(t, err, io.EOF)
}

func TestParseFrame_TruncatedHeaderLine(t *testing.T) {
	t.Parallel()
	// Header line started but no terminator before EOF → ErrInvalidFrame.
	_, err := parseFrame(makeReader("Content-Type: auth/request"))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidFrame)
}

func TestParseFrame_OversizeHeaderRejected(t *testing.T) {
	t.Parallel()
	// 2 MiB of header value — must exceed MaxFrameBytes.
	big := strings.Repeat("a", 2<<20)
	_, err := parseFrame(makeReader("X-Big: " + big + "\n\n"))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidFrame)
}

func TestParseFrame_OversizeBodyRejected(t *testing.T) {
	t.Parallel()
	// Headers fit, but Content-Length pushes consumed past the cap.
	hdr := "Content-Type: api/response\nContent-Length: " + strconv.Itoa(MaxFrameBytes+1) + "\n\n"
	_, err := parseFrame(makeReader(hdr))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidFrame)
}

func TestParseFrame_NoBodyWhenContentLengthAbsent(t *testing.T) {
	t.Parallel()
	frame, err := parseFrame(makeReader("Content-Type: command/reply\nReply-Text: +OK\n\n"))
	require.NoError(t, err)
	require.Nil(t, frame.Body)
}

func TestFrame_AsEventRejectsNonEventFrame(t *testing.T) {
	t.Parallel()
	frame, err := parseFrame(makeReader("Content-Type: command/reply\nReply-Text: +OK\n\n"))
	require.NoError(t, err)

	_, err = frame.AsEvent()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidFrame)
}

func TestFrame_AsEventTolersStrayLines(t *testing.T) {
	t.Parallel()
	// One stray non-colon line is silently dropped; the rest still parses.
	body := "Event-Name: DTMF\nstray-no-colon-line\nUnique-ID: x-y\n\n"
	raw := withBody("Content-Type: text/event-plain\n", body)

	frame, err := parseFrame(makeReader(raw))
	require.NoError(t, err)
	ev, err := frame.AsEvent()
	require.NoError(t, err)
	require.Equal(t, "DTMF", ev.Name)
	require.Equal(t, "x-y", ev.UUID)
}

func TestFrame_AsEventBadURLEncodingFallsBackToRaw(t *testing.T) {
	t.Parallel()
	// "%ZZ" is not valid percent-encoding; QueryUnescape errors and we
	// keep the raw value as a graceful degradation.
	body := "Event-Name: CHANNEL_CREATE\nUnique-ID: x\nVariable-bad: %ZZraw\n\n"
	raw := withBody("Content-Type: text/event-plain\n", body)

	frame, err := parseFrame(makeReader(raw))
	require.NoError(t, err)
	ev, err := frame.AsEvent()
	require.NoError(t, err)
	require.Equal(t, "%ZZraw", ev.Header("Variable-bad"))
}

func TestFrame_HeaderOnZeroValueReturnsEmpty(t *testing.T) {
	t.Parallel()
	// Defensive: zero-value Frame must not panic when callers ask for headers.
	require.Empty(t, Frame{}.Header("X"))
	require.Empty(t, Frame{}.ContentType())
}

func TestEvent_HeaderOnZeroValueReturnsEmpty(t *testing.T) {
	t.Parallel()
	require.Empty(t, Event{}.Header("X"))
}
