package versions

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	metricPollErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "talos_lifecycle_version_poll_errors_total",
		Help: "Failed attempts to fetch or publish upstream release feeds.",
	})

	metricPollLastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "talos_lifecycle_version_poll_last_success_timestamp_seconds",
		Help: "Unix timestamp of the last successful upstream version poll.",
	})

	registerMetricsOnce sync.Once
)

func registerMetrics() {
	registerMetricsOnce.Do(func() {
		metrics.Registry.MustRegister(metricPollErrors, metricPollLastSuccess)
	})
}
