// Package healthz exposes liveness and readiness HTTP endpoints.
//
// /healthz — liveness. Returns 200 once the process is past startup.
// Failing liveness causes Kubernetes to restart the pod, so this handler
// is deliberately trivial: no I/O, no dependencies, no auth.
//
// /readyz — readiness. Reports whether the service can serve traffic right
// now: every registered Checker must return nil within the supplied timeout.
// Failing readiness causes Kubernetes to remove the pod from the Service
// load balancer until checks pass again.
package healthz

import (
	"fmt"
	"net/http"
)

// NewLivenessHandler returns the /healthz handler. Always 200 OK.
func NewLivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})
}
