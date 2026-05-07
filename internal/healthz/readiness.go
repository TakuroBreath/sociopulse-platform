package healthz

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Checker is implemented by every external dependency the gateway must reach
// before serving requests (Postgres, Redis, NATS, …). The Check call is
// expected to be cheap (single ping) and respect the supplied deadline.
type Checker interface {
	Name() string
	Check(ctx context.Context) error
}

// NewReadinessHandler returns an http.Handler that runs every Checker in
// parallel with the supplied timeout. If all return nil, the response is
// 200 + JSON {"status":"ok"}. If any fail, the response is 503 + JSON listing
// per-checker errors and durations.
func NewReadinessHandler(timeout time.Duration, checks ...Checker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		results := runAllParallel(ctx, checks)
		ok := allOK(results)

		w.Header().Set("Content-Type", "application/json")
		if ok {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(buildReport(results))
	})
}

// result captures one Checker's outcome so we can build the final report
// after all goroutines finish.
type result struct {
	name     string
	err      error
	duration time.Duration
}

func runAllParallel(ctx context.Context, checks []Checker) []result {
	out := make([]result, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func(i int, c Checker) {
			defer wg.Done()
			start := time.Now()
			err := c.Check(ctx)
			out[i] = result{
				name:     c.Name(),
				err:      err,
				duration: time.Since(start),
			}
		}(i, c)
	}
	wg.Wait()
	return out
}

func allOK(rs []result) bool {
	for _, r := range rs {
		if r.err != nil {
			return false
		}
	}
	return true
}

type readyReport struct {
	Status string                 `json:"status"`
	Checks map[string]checkReport `json:"checks"`
}

type checkReport struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

func buildReport(rs []result) readyReport {
	rep := readyReport{
		Status: "ok",
		Checks: make(map[string]checkReport, len(rs)),
	}
	for _, r := range rs {
		cr := checkReport{
			OK:         r.err == nil,
			DurationMS: r.duration.Milliseconds(),
		}
		if r.err != nil {
			cr.Error = r.err.Error()
			rep.Status = "fail"
		}
		rep.Checks[r.name] = cr
	}
	return rep
}
