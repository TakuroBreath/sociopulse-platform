package service

import "github.com/prometheus/client_golang/prometheus"

// Metrics is the Authenticator's Prometheus instrumentation. It records
// login outcomes (success / failures-by-reason), account lockout events,
// and refresh-rotation replay detections — the four signals that matter
// for an auth SLO dashboard and security alerting.
//
// Counters are intentionally created un-registered when reg == nil so a
// Metrics value with nil counters never panics. NewMetrics(nil) is the
// canonical way to disable metrics in tests; a real prometheus.Registerer
// is wired in production via the composition root.
type Metrics struct {
	LoginSuccess  prometheus.Counter
	LoginFailures *prometheus.CounterVec // labels: reason
	Locked        prometheus.Counter
	RefreshReplay prometheus.Counter
}

// LoginFailureReason enumerates the reason labels applied to
// Metrics.LoginFailures. The set is closed and low-cardinality so the
// metric is safe to scrape and graph.
const (
	ReasonWrongPassword = "wrong_password"
	ReasonArchived      = "archived"
	ReasonLocked        = "locked"
	ReasonRateLimited   = "rate_limited"
	ReasonUnknown       = "unknown"
	ReasonPwdExpired    = "password_expired"
	ReasonTOTPInvalid   = "totp_invalid"
)

// NewMetrics builds the auth metric set. When reg is nil the counters are
// constructed but not registered with any prometheus.Registerer; .Inc()
// remains safe and increments the in-memory counter (useful in tests).
//
// In production, pass prometheus.DefaultRegisterer (or a per-module
// Registerer) so the counters surface on /metrics. MustRegister is used
// because a duplicate registration is a programming error — the auth
// metrics are owned by the auth module and are constructed exactly once
// at composition time.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		LoginSuccess: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sociopulse_auth_login_success_total",
			Help: "Total successful logins (after TOTP if enabled).",
		}),
		LoginFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sociopulse_auth_login_failure_total",
			Help: "Total failed login attempts, partitioned by reason.",
		}, []string{"reason"}),
		Locked: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sociopulse_auth_account_locked_total",
			Help: "Total times an account was locked due to repeated failures.",
		}),
		RefreshReplay: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sociopulse_auth_refresh_replay_total",
			Help: "Total refresh-token replay detections (rotation reuse).",
		}),
	}
	if reg != nil {
		reg.MustRegister(m.LoginSuccess, m.LoginFailures, m.Locked, m.RefreshReplay)
	}
	return m
}
