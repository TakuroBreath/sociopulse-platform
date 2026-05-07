package healthz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeCheck struct {
	name  string
	err   error
	pause time.Duration
}

func (f fakeCheck) Name() string { return f.name }
func (f fakeCheck) Check(ctx context.Context) error {
	if f.pause > 0 {
		t := time.NewTimer(f.pause)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

func TestReadinessAllOK(t *testing.T) {
	t.Parallel()
	h := NewReadinessHandler(time.Second,
		fakeCheck{name: "postgres"},
		fakeCheck{name: "redis"},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"status":"ok"`)
}

func TestReadinessReportsFailingDependency(t *testing.T) {
	t.Parallel()
	h := NewReadinessHandler(time.Second,
		fakeCheck{name: "postgres", err: errors.New("connection refused")},
		fakeCheck{name: "redis"},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "postgres")
	assert.Contains(t, rec.Body.String(), "connection refused")
}

func TestReadinessTimesOutSlowChecker(t *testing.T) {
	t.Parallel()
	h := NewReadinessHandler(50*time.Millisecond,
		fakeCheck{name: "nats", pause: time.Second},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.True(t, strings.Contains(rec.Body.String(), "deadline") ||
		strings.Contains(rec.Body.String(), "context"))
}

// TestReadinessReportIncludesDuration asserts the JSON report carries a
// per-check duration field so operators can spot slow dependencies.
func TestReadinessReportIncludesDuration(t *testing.T) {
	t.Parallel()
	h := NewReadinessHandler(time.Second,
		fakeCheck{name: "postgres", pause: 10 * time.Millisecond},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Status string `json:"status"`
		Checks map[string]struct {
			OK         bool   `json:"ok"`
			Error      string `json:"error,omitempty"`
			DurationMS int64  `json:"duration_ms"`
		} `json:"checks"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	pg, ok := body.Checks["postgres"]
	require.True(t, ok, "postgres check missing in report: %s", rec.Body.String())
	assert.True(t, pg.OK)
	assert.GreaterOrEqual(t, pg.DurationMS, int64(10),
		"check duration should reflect the 10ms pause, got %dms", pg.DurationMS)
}

// TestReadinessRunsChecksConcurrently asserts that N slow checks finish in
// roughly the time of the slowest one rather than the sum, proving they run in
// parallel.
func TestReadinessRunsChecksConcurrently(t *testing.T) {
	t.Parallel()
	const pause = 80 * time.Millisecond
	h := NewReadinessHandler(time.Second,
		fakeCheck{name: "a", pause: pause},
		fakeCheck{name: "b", pause: pause},
		fakeCheck{name: "c", pause: pause},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	start := time.Now()
	h.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, rec.Code)
	// 3 sequential pauses would be 240ms. Parallel execution should be much
	// closer to a single pause. Allow generous slack for slow CI.
	assert.Less(t, elapsed, 3*pause/2,
		"checks should run in parallel; took %s for 3x %s pauses", elapsed, pause)
}

// TestReadinessEmptyChecksReturnsOK documents the no-deps shape (used early in
// boot before pools are wired) — an empty checker list is healthy.
func TestReadinessEmptyChecksReturnsOK(t *testing.T) {
	t.Parallel()
	h := NewReadinessHandler(time.Second)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"status":"ok"`)
}
