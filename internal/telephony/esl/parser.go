package esl

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

// MaxFrameBytes caps the total bytes consumed by parseFrame for a single
// frame (headers + body). FreeSWITCH events are small in practice
// (a few KiB at most for fully populated CHANNEL_HANGUP_COMPLETE); the 1 MiB
// budget is generous enough that we never reject a real event but tight
// enough that a malicious or corrupted server cannot drive the parser into
// arbitrary memory growth.
const MaxFrameBytes = 1 << 20 // 1 MiB

// Frame represents a single parsed ESL frame: a set of headers plus an
// optional body. Both auth/request, command/reply, api/response, and
// text/event-* frames map to this shape — the discriminator is the
// Content-Type header.
//
// Frames are immutable after parseFrame returns; callers must not mutate
// the headers map.
type Frame struct {
	// headers stores lowercased header names → trimmed values. Lowercase
	// keys make Header() lookups case-insensitive without per-call
	// allocation. Unexported because the canonical accessor is Header().
	headers map[string]string

	// Body is the raw frame body, sized to Content-Length. Nil when the
	// frame carries no body.
	Body []byte
}

// Header returns the value of the named header in case-insensitive form.
// Returns the empty string if the header is absent.
func (f Frame) Header(name string) string {
	if f.headers == nil {
		return ""
	}
	return f.headers[strings.ToLower(name)]
}

// ContentType returns the Content-Type header (already lowercased on store
// but the values FS sends are themselves already lowercase tokens like
// "text/event-plain", so the round-trip preserves them verbatim).
func (f Frame) ContentType() string {
	return f.Header("Content-Type")
}

// parseFrame reads one ESL frame from r: header lines terminated by an
// empty line, followed by Content-Length bytes of body if present.
//
// The function bounds total bytes at MaxFrameBytes and returns
// ErrInvalidFrame for malformed inputs (missing colon, non-numeric
// Content-Length, oversized header section). io.ErrUnexpectedEOF and
// io.EOF surface verbatim so callers can distinguish "server hung up" from
// "server sent garbage".
func parseFrame(r *bufio.Reader) (Frame, error) {
	headers := make(map[string]string, 16)
	var consumed int

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if len(line) == 0 {
				return Frame{}, fmt.Errorf("read header: %w", err)
			}
			// Partial header line at EOF — surface as invalid so the
			// caller knows the server cut us off mid-frame.
			return Frame{}, fmt.Errorf("%w: truncated header line %q", ErrInvalidFrame, line)
		}
		consumed += len(line)
		if consumed > MaxFrameBytes {
			return Frame{}, fmt.Errorf("%w: header section exceeds %d bytes", ErrInvalidFrame, MaxFrameBytes)
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}

		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			return Frame{}, fmt.Errorf("%w: malformed header %q", ErrInvalidFrame, line)
		}
		name := strings.ToLower(strings.TrimSpace(line[:idx]))
		value := strings.TrimSpace(line[idx+1:])
		headers[name] = value
	}

	frame := Frame{headers: headers}

	cl, ok := headers["content-length"]
	if !ok || cl == "" {
		return frame, nil
	}

	n, err := strconv.Atoi(cl)
	if err != nil {
		return Frame{}, fmt.Errorf("%w: parse content-length: %w", ErrInvalidFrame, err)
	}
	if n < 0 {
		return Frame{}, fmt.Errorf("%w: negative content-length %d", ErrInvalidFrame, n)
	}
	if consumed+n > MaxFrameBytes {
		return Frame{}, fmt.Errorf("%w: body of %d bytes exceeds frame budget", ErrInvalidFrame, n)
	}

	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return Frame{}, fmt.Errorf("read body: %w", err)
	}
	frame.Body = body
	return frame, nil
}

// Event is a decoded FreeSWITCH event: name + UUID + the full header bag
// from the event body. The headers are URL-decoded; FS URL-encodes spaces,
// colons, and non-ASCII payload values inside event bodies (but NOT in
// frame headers).
type Event struct {
	// Name is the FS event name, e.g. "CHANNEL_CREATE",
	// "CHANNEL_HANGUP_COMPLETE".
	Name string

	// UUID is the channel UUID (Unique-ID header). May be empty for
	// non-channel events such as BACKGROUND_JOB or sofia::register.
	UUID string

	// headers is the full per-event header bag, lowercased keys.
	headers map[string]string
}

// Header returns the named event-body header in case-insensitive form.
func (e Event) Header(name string) string {
	if e.headers == nil {
		return ""
	}
	return e.headers[strings.ToLower(name)]
}

// AsEvent decodes a text/event-plain frame body into an Event. Returns
// ErrInvalidFrame when invoked on a non-event content-type.
//
// The event body wire format is "Header: value\nHeader: value\n\n", with
// values URL-encoded. We decode each value; URL-decode failures fall back
// to the raw value (some FS releases double-encode special chars; better
// to surface the raw bytes than drop the field).
func (f Frame) AsEvent() (Event, error) {
	ct := f.ContentType()
	if !strings.Contains(ct, "event") {
		return Event{}, fmt.Errorf("%w: not an event frame (content-type=%q)", ErrInvalidFrame, ct)
	}

	headers := make(map[string]string, 32)
	for line := range strings.SplitSeq(string(f.Body), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			// Tolerate stray non-header lines silently — FS occasionally
			// emits free-form trailers (e.g. SIP fragments). Hard-failing
			// on these would lose otherwise-valid events.
			continue
		}
		name := strings.ToLower(strings.TrimSpace(line[:idx]))
		value := strings.TrimSpace(line[idx+1:])
		if dec, err := url.QueryUnescape(value); err == nil {
			value = dec
		}
		headers[name] = value
	}

	return Event{
		Name:    headers["event-name"],
		UUID:    headers["unique-id"],
		headers: headers,
	}, nil
}
