//go:build smoke

package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// defaultHTTPTimeout is the per-request budget for smoke HTTP calls.
// 5 s comfortably covers a slow CI runner's TCP+TLS handshake (sub-100 ms
// typical) while keeping a failing test snappy. Override per-test via a
// custom *http.Client when a scenario genuinely needs longer (e.g. an
// async report export polling loop).
const defaultHTTPTimeout = 5 * time.Second

// MustGet performs an HTTP GET against url and returns the response.
// Test fails on transport error. The caller MUST close resp.Body
// (typically via t.Cleanup(func(){ _ = resp.Body.Close() })).
//
// ctx is honoured for cancellation: when ctx is cancelled the GET
// returns context.Canceled. Pass t.Context() in most cases — it is
// automatically cancelled at test end.
func MustGet(ctx context.Context, t *testing.T, url string) *http.Response {
	t.Helper()
	cli := &http.Client{Timeout: defaultHTTPTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err, "smoke: build GET %s", url)
	resp, err := cli.Do(req)
	require.NoError(t, err, "smoke: GET %s", url)
	return resp
}

// DoRequest issues an HTTP request and returns the response plus the
// fully-consumed body (body is closed before return).
//
// body=nil means no request body. body of type []byte is sent verbatim
// with no Content-Type header — the caller MUST set Content-Type via
// hdrs when the receiver requires one (typically application/json).
// body of any other type is JSON-encoded; the Content-Type header is
// set automatically unless hdrs already provides one.
//
// hdrs=nil is fine (no extra headers). When hdrs sets Authorization,
// the receiver decides how to validate it.
//
// Test fails on transport / build / read error. HTTP-level errors (4xx,
// 5xx) are returned via resp.StatusCode + body for the caller to assert.
func DoRequest(
	ctx context.Context,
	t *testing.T,
	method, url string,
	body any,
	hdrs map[string]string,
) (*http.Response, []byte) {
	t.Helper()

	var (
		bodyReader io.Reader
		isJSON     bool
	)
	switch v := body.(type) {
	case nil:
		// no body
	case []byte:
		bodyReader = bytes.NewReader(v)
	case string:
		bodyReader = bytes.NewReader([]byte(v))
	default:
		buf, err := json.Marshal(body)
		require.NoError(t, err, "smoke: marshal body for %s %s", method, url)
		bodyReader = bytes.NewReader(buf)
		isJSON = true
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	require.NoError(t, err, "smoke: build %s %s", method, url)

	if isJSON {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}

	cli := &http.Client{Timeout: defaultHTTPTimeout}
	resp, err := cli.Do(req)
	require.NoError(t, err, "smoke: %s %s", method, url)
	defer func() { _ = resp.Body.Close() }()

	buf, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "smoke: read body for %s %s", method, url)
	return resp, buf
}

// RequireJSON decodes resp.Body into target. Asserts the response was
// 200-class; non-2xx codes surface via t.Fatalf with the body so the
// failure message is diagnostic. The caller is responsible for closing
// resp.Body separately (RequireJSON does not close it).
//
// Use DoRequest when you also need the raw bytes; RequireJSON is the
// convenience path for the common "decode JSON or fail" case.
func RequireJSON(t *testing.T, resp *http.Response, target any) {
	t.Helper()
	if resp.StatusCode/100 != 2 {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("smoke: expected 2xx, got %d; body=%s", resp.StatusCode, string(buf))
	}
	err := json.NewDecoder(resp.Body).Decode(target)
	require.NoError(t, err, "smoke: decode JSON body")
}

// ParseError tries to extract the error message from a non-2xx JSON
// response of the shape `{"error":"..."}` or `{"message":"..."}`.
// Returns the raw body when the shape doesn't match. Useful for
// diagnosing assertion failures without dumping huge response payloads.
func ParseError(body []byte) string {
	var shape struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		return fmt.Sprintf("(non-JSON body, %d bytes)", len(body))
	}
	switch {
	case shape.Error != "":
		return shape.Error
	case shape.Message != "":
		return shape.Message
	default:
		return fmt.Sprintf("(unrecognised error shape, %d bytes)", len(body))
	}
}
