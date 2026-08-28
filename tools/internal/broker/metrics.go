package broker

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Prometheus metrics for the Phase 5 observability slice. Registered on
// the default registry; Routes serves them at /metrics on the
// management listener. Per-tenant label cardinality is bounded by
// provisioning.maxTenants.
var (
	metricTenants = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "demarkus_broker_tenants",
		Help: "Dynamic tenant worlds currently in the registry.",
	})
	metricProvisioning = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "demarkus_broker_provisioning_total",
		Help: "Tenant provisioning attempts by result (created, converged, denied, capacity, error).",
	}, []string{"result"})
	metricSoulSeedFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "demarkus_broker_soul_seed_failures_total",
		Help: "Failed soul-template seeding attempts (retried later).",
	})
	metricTenantDenials = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "demarkus_broker_tenant_denials_total",
		Help: "Tenant-gate denials by reason (unresolved, cross_tenant, unclassified_tool).",
	}, []string{"reason"})
	metricTenantActivity = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "demarkus_broker_tenant_last_activity_seconds",
		Help: "Unix time of the tenant's last gated tool call; the dormancy signal (policy stays explicit, nothing auto-expires).",
	}, []string{"world"})
)

// metricsHandler serves the default Prometheus registry.
func metricsHandler() http.Handler {
	return promhttp.Handler()
}
