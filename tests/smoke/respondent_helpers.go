//go:build smoke

package smoke

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// csvImportHeader mirrors the canonical column order
// internal/crm/service/import_csv.go::canonicalHeaderAliases recognises:
// phone (mandatory) + full_name + external_ref. The production parser
// is case-insensitive and accepts Russian aliases; we use the
// canonical English forms so the bytes are equally readable in
// pg_stat / docker logs.
//
// Order matters because BuildCSVImport feeds the rows positionally —
// each input row's [0] becomes phone, [1] becomes full_name, [2]
// becomes external_ref. Callers that need a different column shape
// should construct the CSV bytes themselves rather than re-shaping the
// helper.
var csvImportHeader = []string{"phone", "full_name", "external_ref"}

// BuildCSVImport renders rows as a UTF-8 CSV body that
// internal/crm/service/import_csv.go::parseCSV will accept. Each input
// row is written verbatim under csvImportHeader; rows shorter than the
// header pad with empty strings so the output stays rectangular and
// the canonical CSV reader can parse it without LazyQuotes triggering.
//
// The returned bytes carry NO BOM (the production parser strips it,
// but the smoke harness produces clean Unix-style CSV for diagnostic
// readability — a curl --data-binary on the bytes round-trips through
// any text editor).
//
// Errors are NOT returned: every input shape encoding/csv accepts is a
// success here. A future caller that wants to assert a deliberate
// parse failure should construct the bad-CSV bytes directly.
func BuildCSVImport(rows [][]string) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	// encoding/csv.Writer.Write returns an error only on the underlying
	// io.Writer's Write — bytes.Buffer is infallible, so we ignore the
	// error returns to keep the helper signature clean. A defensive
	// Flush() at the end surfaces any deferred errno via Error()
	// (still infallible for bytes.Buffer, but defends against the
	// helper being adapted to a different writer in the future).
	_ = w.Write(csvImportHeader)
	for _, row := range rows {
		padded := make([]string, len(csvImportHeader))
		copy(padded, row)
		_ = w.Write(padded)
	}
	w.Flush()
	return buf.Bytes()
}

// importStatusBody mirrors the subset of internal/crm/transport/http/
// dto.go::ImportStatusDTO the polling helper reads. We decode into a
// local struct (rather than importing the dto type) so the harness
// stays free of cross-module imports — the wire shape is the contract,
// and a future DTO field rename surfaces here as a missing JSON tag.
type importStatusBody struct {
	JobID string `json:"job_id"`
	State string `json:"state"`
	Total int    `json:"total"`
}

// terminalImportStates is the set of "the job is done" markers the
// poll loop accepts as exit conditions. Mirrors
// internal/crm/service/import_progress.go's stateSucceeded /
// stateFailed constants. We accept BOTH because a scenario asserts
// the desired outcome separately — WaitForImportStatus only proves
// the job reached a final state.
var terminalImportStates = map[string]struct{}{
	"succeeded": {},
	"failed":    {},
}

// importPollInterval is how often WaitForImportStatus re-checks
// /api/imports/:job_id. 250 ms keeps cold-test latency low (a happy-
// path import processes in well under 1 s on a warm container) while
// staying loose enough not to hammer the gateway during a deliberately
// slow scenario.
const importPollInterval = 250 * time.Millisecond

// WaitForImportStatus polls GET /api/imports/:jobID under the supplied
// JWT until the response's State field equals target (or any terminal
// state when target is empty / "*"). Surfaces the final status to the
// caller via t.Logf so scenario diagnostics retain the row counts
// without forcing the helper to return a struct.
//
// addr is the cmd/api HTTP listener ("127.0.0.1:NNNN"); jwt is the
// admin's access_token (operator role does NOT have access to the
// import status endpoint per the RBAC matrix).
//
// Failure modes (all surface via t.Fatalf):
//   - Polling deadline reached — defaults to 30 s, overridable in a
//     future variant by extending t.Context()'s timeout before calling.
//   - Non-2xx response that is also non-404 — a 404 is treated as
//     "not yet visible to the gateway" and retried; everything else
//     fails fast with the body for diagnostics.
//   - JSON decode failure on a 200 body — implies a DTO drift; surfaces
//     the bytes for a fast triage.
//
// The helper uses time.NewTicker (not time.After in a for-loop —
// see make grep-time-after) so retry deadlines do not leak timers.
func WaitForImportStatus(t *testing.T, addr, jwt, jobID, target string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	url := "http://" + addr + "/api/imports/" + jobID
	cli := &http.Client{Timeout: 5 * time.Second}

	tick := time.NewTicker(importPollInterval)
	defer tick.Stop()

	check := func() (importStatusBody, bool) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		require.NoError(t, err, "smoke: build GET %s", url)
		req.Header.Set("Authorization", "Bearer "+jwt)

		resp, err := cli.Do(req)
		if err != nil {
			// Transport-level error — surface and retry; ctx cancellation
			// will eventually break the loop.
			t.Logf("smoke import poll: transport err %v (retrying)", err)
			return importStatusBody{}, false
		}
		defer func() { _ = resp.Body.Close() }()

		switch resp.StatusCode {
		case http.StatusNotFound:
			// Job hasn't propagated to the status endpoint yet.
			return importStatusBody{}, false
		case http.StatusOK:
			var st importStatusBody
			if derr := json.NewDecoder(resp.Body).Decode(&st); derr != nil {
				t.Fatalf("smoke import poll: decode body: %v", derr)
			}
			return st, isTerminal(st.State, target)
		default:
			body := readBodyForError(resp)
			t.Fatalf("smoke import poll: GET %s unexpected status %d body=%s",
				url, resp.StatusCode, body)
			return importStatusBody{}, false
		}
	}

	// Fire one immediate check so the happy path doesn't pay the first
	// tick interval as added latency.
	if st, ok := check(); ok {
		t.Logf("smoke import poll: terminal state %q reached (job=%s, total=%d)", st.State, st.JobID, st.Total)
		return
	}

	for {
		select {
		case <-ctx.Done():
			t.Fatalf("smoke import poll: timeout (target=%q): %v", target, ctx.Err())
			return
		case <-tick.C:
			if st, ok := check(); ok {
				t.Logf("smoke import poll: terminal state %q reached (job=%s, total=%d)", st.State, st.JobID, st.Total)
				return
			}
		}
	}
}

// isTerminal returns true when state is the explicit target, or — when
// target is empty/"*" — when state appears in the terminalImportStates
// set. Keeps the polling loop's exit condition declarative.
func isTerminal(state, target string) bool {
	if target == "" || target == "*" {
		_, ok := terminalImportStates[state]
		return ok
	}
	return state == target
}

// readBodyForError reads up to 4 KiB of resp.Body for a diagnostic.
// The body is otherwise discarded — the t.Fatalf consumer gets a short
// hint without paging in megabytes of accidental dump.
func readBodyForError(resp *http.Response) string {
	const cap = 4096
	buf := make([]byte, cap)
	n, _ := resp.Body.Read(buf)
	return fmt.Sprintf("%s", buf[:n])
}
