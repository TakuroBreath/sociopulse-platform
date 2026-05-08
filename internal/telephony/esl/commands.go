package esl

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
	"time"
)

// High-level ESL commands built on top of sendCommand. Each command is
// defined as a method on *Client; arguments use package-private request
// types (e.g. OriginateRequest) that are intentionally distinct from
// the public api.OriginateCommand DTO in internal/telephony/api — Task 4
// owns the boundary that converts from one to the other so this package
// stays free of cross-module imports.
//
// Notes on the FS wire protocol used here:
//
//   - `bgapi <verb>` returns a synchronous `command/reply` whose
//     Reply-Text on success is `+OK <Job-UUID>`. The actual call UUID
//     for `originate` arrives later via a BACKGROUND_JOB event. Some FS
//     builds shortcut and return the call UUID directly in the +OK
//     payload. Plan 09 Task 3 returns whatever +OK token FS sends — Task
//     4 (pool) and Task 6 (reconciler) are the natural seams to add
//     proper Job-UUID-by-event correlation when we have reliable FS
//     integration tests in place.
//     FIXME(plan-09): wire BACKGROUND_JOB correlation in Task 4 / Task 6.
//
//   - `api <verb>` is synchronous and returns `api/response` with the
//     result text in the BODY (not Reply-Text). SofiaStatus uses this.
//
//   - CreateUser / DeleteUser are NOT direct ESL commands — they route
//     through `mod_xml_curl` via the /internal/freeswitch/directory
//     HTTP endpoint owned by cmd/api (separate concern, Task 4-5).
//     ReloadXMLDirectory is the lever this package exposes to invalidate
//     the FS-side directory cache after a Redis credential update.

// OriginateRequest carries the parameters of a `bgapi originate` call.
// All fields except CallURL are optional. Defaults are filled in by
// Originate.
type OriginateRequest struct {
	// CallURL is the originate destination, e.g.
	// "sofia/gateway/<gw-name>/<dest>" or any FS-recognised URL.
	// Required — Originate returns an error when empty.
	CallURL string

	// Extension is the dialplan action executed once the leg answers.
	// Examples: "&park()", "&bridge(sofia/internal/100)", or a dialplan
	// extension name. Empty defaults to "&park()".
	Extension string

	// Caller is the origination caller-id number, surfaced to FS as the
	// channel variable origination_caller_id_number.
	Caller string

	// CallerName is the origination caller-id display name, surfaced as
	// origination_caller_id_name.
	CallerName string

	// Variables is an arbitrary set of channel variables to inject into
	// the originate `{var=val,…}` prefix. Keys are sorted before
	// serialisation so the wire format is deterministic across runs.
	Variables map[string]string

	// Timeout is the per-leg originate timeout. Zero defaults to 30s.
	// Rounded to whole seconds (FS originate_timeout is an integer).
	Timeout time.Duration
}

// Originate issues a `bgapi originate {vars}<call-url> <extension>`
// command and returns the +OK payload from FS (call-UUID or Job-UUID
// depending on FS build).
//
// Returns ErrCommandFailed wrapped with the FS reply when FS responds
// with `-ERR …`. See the package-level FIXME(plan-09) note for the
// BACKGROUND_JOB correlation deferral.
//
// References: https://developer.signalwire.com/freeswitch/FreeSWITCH-Explained/Dialplan/originate_3375460/
func (c *Client) Originate(ctx context.Context, req OriginateRequest) (string, error) {
	if req.CallURL == "" {
		return "", fmt.Errorf("%w: call_url required", ErrInvalidArgument)
	}
	if req.Extension == "" {
		req.Extension = "&park()"
	}
	if req.Timeout == 0 {
		req.Timeout = 30 * time.Second
	}

	vars := buildVariables(req)
	cmd := fmt.Sprintf("bgapi originate %s%s %s", vars, req.CallURL, req.Extension)

	frame, err := c.sendCommand(ctx, cmd)
	if err != nil {
		return "", err
	}

	payload := replyPayload(frame)
	ok, body := splitOKBody(payload)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrCommandFailed, body)
	}
	return body, nil
}

// Hangup issues `bgapi uuid_kill <uuid> <cause>`. cause defaults to
// "NORMAL_CLEARING" when empty. uuid is required.
//
// References: https://developer.signalwire.com/freeswitch/FreeSWITCH-Explained/Dialplan/uuid_kill_3375468/
func (c *Client) Hangup(ctx context.Context, uuid, cause string) error {
	if uuid == "" {
		return fmt.Errorf("%w: uuid required", ErrInvalidArgument)
	}
	if cause == "" {
		cause = "NORMAL_CLEARING"
	}
	return c.sendOKCommand(ctx, fmt.Sprintf("bgapi uuid_kill %s %s", uuid, cause))
}

// MixMonitorStart issues `bgapi uuid_record <uuid> start <path> [flags]`.
// flags is comma-joined when non-empty, e.g. ["stereo","mux"] →
// "stereo,mux". uuid and path are required.
//
// References: https://developer.signalwire.com/freeswitch/FreeSWITCH-Explained/Dialplan/uuid_record_3375473/
func (c *Client) MixMonitorStart(ctx context.Context, uuid, path string, flags []string) error {
	if uuid == "" || path == "" {
		return fmt.Errorf("%w: uuid and path required", ErrInvalidArgument)
	}
	cmd := fmt.Sprintf("bgapi uuid_record %s start %s", uuid, path)
	if len(flags) > 0 {
		cmd += " " + strings.Join(flags, ",")
	}
	return c.sendOKCommand(ctx, cmd)
}

// MixMonitorStop issues `bgapi uuid_record <uuid> stop <path>`. uuid
// and path are required.
//
// References: https://developer.signalwire.com/freeswitch/FreeSWITCH-Explained/Dialplan/uuid_record_3375473/
func (c *Client) MixMonitorStop(ctx context.Context, uuid, path string) error {
	if uuid == "" || path == "" {
		return fmt.Errorf("%w: uuid and path required", ErrInvalidArgument)
	}
	return c.sendOKCommand(ctx, fmt.Sprintf("bgapi uuid_record %s stop %s", uuid, path))
}

// Play issues `bgapi uuid_broadcast <uuid> <path> aleg`. uuid and path
// are required. The "aleg" leg targets the originating side (operator);
// callers wanting bleg or both should issue uuid_broadcast directly via
// a future helper.
//
// References: https://developer.signalwire.com/freeswitch/FreeSWITCH-Explained/Dialplan/uuid_broadcast_3375458/
func (c *Client) Play(ctx context.Context, uuid, path string) error {
	if uuid == "" || path == "" {
		return fmt.Errorf("%w: uuid and path required", ErrInvalidArgument)
	}
	return c.sendOKCommand(ctx, fmt.Sprintf("bgapi uuid_broadcast %s %s aleg", uuid, path))
}

// SofiaStatus issues a synchronous `api sofia status`. The full body of
// the api/response frame is returned verbatim — used by the pool
// supervisor's health check (Task 4) to confirm mod_sofia is alive.
//
// References: https://developer.signalwire.com/freeswitch/FreeSWITCH-Explained/Modules/mod_sofia_1048898/
func (c *Client) SofiaStatus(ctx context.Context) (string, error) {
	frame, err := c.sendCommand(ctx, "api sofia status")
	if err != nil {
		return "", err
	}
	return string(frame.Body), nil
}

// ChannelsCount issues a synchronous `api show channels count` and parses
// the integer prefix from the FS response body. Used by the reconciler
// (router.Reconciler, Plan 09 Task 6) to fetch the ground-truth active-
// channel count for drift correction against Redis.
//
// FS body format on a healthy build is "N total.\n" — we consume the first
// whitespace-separated token and strconv.Atoi it. An empty body (some FS
// builds emit just "\n" when no channels exist) returns 0 with no error so
// callers do not need to special-case the idle node.
//
// References: https://developer.signalwire.com/freeswitch/FreeSWITCH-Explained/Modules/mod_commands_1966741/#show
func (c *Client) ChannelsCount(ctx context.Context) (int, error) {
	frame, err := c.sendCommand(ctx, "api show channels count")
	if err != nil {
		return 0, err
	}
	body := strings.TrimSpace(string(frame.Body))
	if body == "" {
		return 0, nil
	}
	// strings.Fields on a non-empty trimmed string always returns ≥1
	// element, so we don't re-check len(fields) here.
	first := strings.Fields(body)[0]
	n, err := strconv.Atoi(first)
	if err != nil {
		return 0, fmt.Errorf("%w: parse channels count %q: %w", ErrCommandFailed, first, err)
	}
	return n, nil
}

// SubscribeEvents issues `event plain <e1> <e2> …`. An empty events
// slice is a no-op (returns nil without writing to the wire). Validates
// the FS Reply-Text starts with `+`; anything else maps to
// ErrCommandFailed.
//
// References: https://developer.signalwire.com/freeswitch/FreeSWITCH-Explained/Modules/mod_event_socket_1048924/
func (c *Client) SubscribeEvents(ctx context.Context, events []string) error {
	if len(events) == 0 {
		return nil
	}
	cmd := "event plain " + strings.Join(events, " ")
	frame, err := c.sendCommand(ctx, cmd)
	if err != nil {
		return err
	}
	reply := strings.TrimLeft(frame.Header("Reply-Text"), " \t")
	if reply == "" || reply[0] != '+' {
		return fmt.Errorf("%w: %s", ErrCommandFailed, reply)
	}
	return nil
}

// ReloadXMLDirectory invalidates FS's cached directory XML. With a
// non-empty domain it issues `api xml_flush_cache <domain>` (per-domain
// scoped flush, cheaper than a full reload); empty domain falls back to
// `api reloadxml` which re-parses every XML file FS is configured to
// load.
//
// Used by cmd/api's /internal/freeswitch/directory route to force FS
// to re-fetch a SIP credential after Redis update.
//
// References: https://developer.signalwire.com/freeswitch/FreeSWITCH-Explained/Modules/mod_xml_curl_1049004/
func (c *Client) ReloadXMLDirectory(ctx context.Context, domain string) error {
	cmd := "api reloadxml"
	if domain != "" {
		cmd = "api xml_flush_cache " + domain
	}
	frame, err := c.sendCommand(ctx, cmd)
	if err != nil {
		return err
	}
	payload := replyPayload(frame)
	if ok, _ := splitOKBody(payload); !ok {
		return fmt.Errorf("%w: %s", ErrCommandFailed, payload)
	}
	return nil
}

// sendOKCommand is the shared shape for fire-and-acknowledge commands —
// it writes the command, then expects a +OK Reply-Text or body. Any
// other shape maps to ErrCommandFailed wrapping the FS reply.
//
// Pulled out so each high-level method stays linear and the wrapping
// behaviour is asserted in one place.
func (c *Client) sendOKCommand(ctx context.Context, cmd string) error {
	frame, err := c.sendCommand(ctx, cmd)
	if err != nil {
		return err
	}
	payload := replyPayload(frame)
	if ok, _ := splitOKBody(payload); !ok {
		return fmt.Errorf("%w: %s", ErrCommandFailed, payload)
	}
	return nil
}

// replyPayload returns the most informative status string from a frame.
// FS returns +OK / -ERR in two places depending on the verb:
//   - bgapi → command/reply with Reply-Text: +OK <Job-UUID>.
//   - api   → api/response with the result in Body.
//
// We prefer Body when present (the api-response shape) and fall back
// to Reply-Text otherwise. Trim leading whitespace because FS sometimes
// pads Reply-Text (" +OK …").
func replyPayload(f Frame) string {
	if len(f.Body) > 0 {
		return strings.TrimSpace(string(f.Body))
	}
	return strings.TrimLeft(f.Header("Reply-Text"), " \t")
}

// splitOKBody decodes a "+OK [body]" / "-ERR …" status line. Returns
// (true, body) when the input has a +OK prefix; the body is whatever
// follows the +OK token, trimmed. Returns (false, raw) for everything
// else so the caller can wrap raw into ErrCommandFailed verbatim.
func splitOKBody(s string) (bool, string) {
	if !strings.HasPrefix(s, "+OK") {
		return false, s
	}
	rest := strings.TrimSpace(strings.TrimPrefix(s, "+OK"))
	return true, rest
}

// buildVariables serialises req's caller / variables / timeout into the
// FS originate `{var=val,…}` channel-variable prefix. Returns the empty
// string when no variables are set, so the caller can concatenate
// unconditionally.
//
// Keys are sorted to give deterministic output (essential for tests
// asserting the exact bytes on the wire). Values are passed through
// escapeVar to strip braces and escape commas — the FS originate parser
// treats both as control characters within the prefix.
func buildVariables(req OriginateRequest) string {
	vars := make(map[string]string, len(req.Variables)+3)
	maps.Copy(vars, req.Variables)
	if req.Caller != "" {
		vars["origination_caller_id_number"] = req.Caller
	}
	if req.CallerName != "" {
		vars["origination_caller_id_name"] = req.CallerName
	}
	if req.Timeout > 0 {
		vars["originate_timeout"] = strconv.Itoa(int(req.Timeout.Round(time.Second).Seconds()))
	}
	if len(vars) == 0 {
		return ""
	}

	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.Grow(2 + len(vars)*32)
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(escapeVar(vars[k]))
	}
	b.WriteByte('}')
	return b.String()
}

// escapeVar prepares a channel-variable value for inclusion inside the
// FS originate `{…}` prefix. FS does not support arbitrary character
// escaping there: we strip braces (which would prematurely close the
// prefix) and backslash-escape commas (which separate variables).
//
// Real production callers should validate inputs upstream — this helper
// is a defence-in-depth net.
func escapeVar(v string) string {
	v = strings.ReplaceAll(v, "{", "")
	v = strings.ReplaceAll(v, "}", "")
	v = strings.ReplaceAll(v, ",", "\\,")
	return v
}
