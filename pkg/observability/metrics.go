package observability

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sociopulse/platform/pkg/config"
)

// Metrics groups every Prometheus collector cmd/api owns. Each module that
// exports metrics registers them through Metrics.Register so namespacing and
// constant labels are uniform.
//
// The registry is intentionally NOT prometheus.DefaultRegisterer — keeping it
// scoped lets tests build fresh instances without leaking metrics across runs
// and avoids global mutable state.
type Metrics struct {
	Registry  *prometheus.Registry
	Namespace string

	HTTPRequestDuration *prometheus.HistogramVec
	HTTPInflight        prometheus.Gauge
	HTTPRequestsTotal   *prometheus.CounterVec
	WSConnectionsActive *prometheus.GaugeVec
	NATSMessagesIn      *prometheus.CounterVec
	NATSMessagesOut     *prometheus.CounterVec
	DBConnsActive       prometheus.Gauge
	DBQueryDuration     *prometheus.HistogramVec
}

// NewMetrics builds the Prometheus registry and registers the Go runtime +
// process collectors under the configured namespace. Business metrics live
// inside individual modules; this struct holds the cross-cutting ones.
func NewMetrics(cfg config.Config) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	ns := cfg.Observability.Metrics.Namespace

	m := &Metrics{
		Registry:  reg,
		Namespace: ns,
		HTTPRequestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: ns,
				Name:      "http_request_duration_seconds",
				Help:      "Latency of HTTP requests handled by gateway middleware.",
				Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
			},
			[]string{"method", "path", "status"},
		),
		HTTPInflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "http_inflight_requests",
			Help:      "In-flight HTTP requests.",
		}),
		HTTPRequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: ns,
				Name:      "http_requests_total",
				Help:      "Total HTTP requests served, partitioned by method, path, and status.",
			},
			[]string{"method", "path", "status"},
		),
		WSConnectionsActive: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: ns,
				Name:      "ws_connections_active",
				Help:      "Active WebSocket connections per tenant.",
			},
			[]string{"tenant_id"},
		),
		NATSMessagesIn: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: ns,
				Name:      "nats_messages_in_total",
				Help:      "Inbound NATS messages by subject prefix.",
			},
			[]string{"subject"},
		),
		NATSMessagesOut: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: ns,
				Name:      "nats_messages_out_total",
				Help:      "Outbound NATS messages by subject prefix.",
			},
			[]string{"subject"},
		),
		DBConnsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "db_connections_active",
			Help:      "Active Postgres connections.",
		}),
		DBQueryDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: ns,
				Name:      "db_query_duration_seconds",
				Help:      "Postgres query latency.",
				Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
			},
			[]string{"query"},
		),
	}
	reg.MustRegister(
		m.HTTPRequestDuration,
		m.HTTPInflight,
		m.HTTPRequestsTotal,
		m.WSConnectionsActive,
		m.NATSMessagesIn,
		m.NATSMessagesOut,
		m.DBConnsActive,
		m.DBQueryDuration,
	)
	return m
}

// Handler returns a handler suitable for mounting at /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{
		Timeout:           5 * time.Second,
		EnableOpenMetrics: true,
	})
}

// Register lets a module add additional collectors under the same registry.
func (m *Metrics) Register(c prometheus.Collector) error {
	return m.Registry.Register(c)
}

// MustRegister panics on registration error. Modules that have a static set of
// metrics use this at init time so misconfiguration is loud.
func (m *Metrics) MustRegister(c ...prometheus.Collector) {
	m.Registry.MustRegister(c...)
}
