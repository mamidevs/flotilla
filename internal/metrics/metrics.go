// Package metrics defines all Prometheus collectors used by Flotilla.
// All metric names are prefixed with `flotilla_`.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Registry returns the default Prometheus registry. Tests can override.
var Registry = prometheus.DefaultRegisterer

var (
	requestsTotal = promauto.With(Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "flotilla_requests_total",
			Help: "Total proxied requests, partitioned by node and protocol.",
		},
		[]string{"node", "protocol", "result"},
	)

	requestDuration = promauto.With(Registry).NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "flotilla_request_duration_seconds",
			Help:    "Proxied request duration in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"node", "protocol"},
	)

	activeConnections = promauto.With(Registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "flotilla_active_connections",
			Help: "In-flight proxied connections per node.",
		},
		[]string{"node"},
	)

	nodeUp = promauto.With(Registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "flotilla_node_up",
			Help: "Worker health (1 = healthy, 0 = unhealthy).",
		},
		[]string{"node"},
	)

	nodeEgressIPInfo = promauto.With(Registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "flotilla_node_egress_ip_info",
			Help: "One-hot metric carrying the egress IP as a label value.",
		},
		[]string{"node", "egress_ip"},
	)

	poolPicks = promauto.With(Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "flotilla_pool_pick_total",
			Help: "Total pool.Pick decisions, partitioned by strategy and node.",
		},
		[]string{"strategy", "node", "result"},
	)
)

// Now returns the current time. Centralized so tests can hook in fakeclock.
func Now() time.Time { return time.Now() }

// ObserveRequest is called by the dispatcher after each proxied dial.
func ObserveRequest(node, protocol, result string, started time.Time) {
	requestsTotal.WithLabelValues(node, protocol, result).Inc()
	requestDuration.WithLabelValues(node, protocol).Observe(time.Since(started).Seconds())
}

// ObservePick is called for every pool.Pick result.
func ObservePick(strategy, node string, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	poolPicks.WithLabelValues(strategy, node, result).Inc()
}

// SetActiveConnections is called by an admin refresh loop.
func SetActiveConnections(node string, active int64) {
	activeConnections.WithLabelValues(node).Set(float64(active))
}

// SetNodeHealth flips the per-node health gauge.
func SetNodeHealth(node string, healthy bool) {
	v := 0.0
	if healthy {
		v = 1.0
	}
	nodeUp.WithLabelValues(node).Set(v)
}

// SetEgressIPInfo records the current egress IP as a label value. We delete
// any previous label combination for the same node first so old IPs don't
// linger after a node's exit changes.
func SetEgressIPInfo(node, ip string) {
	nodeEgressIPInfo.DeletePartialMatch(prometheus.Labels{"node": node})
	if ip != "" {
		nodeEgressIPInfo.WithLabelValues(node, ip).Set(1)
	}
}
