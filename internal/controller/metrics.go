package controller

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

var (
	metricPlanFrozen = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "talos_lifecycle_plan_frozen",
		Help: "1 while a failed upgrade freezes the plan, 0 otherwise.",
	})

	metricPlanBlocked = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_lifecycle_plan_blocked",
		Help: "1 for the reason the plan waits on an operator (incompatible or unsupported versions, downgrade, refused upgrade).",
	}, []string{"reason"})
	metricNodes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_lifecycle_nodes",
		Help: "Number of cluster nodes per upgrade phase.",
	}, []string{"phase"})

	metricUpgradesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "talos_lifecycle_upgrades_total",
		Help: "Finished upgrades by type (talos|kubernetes) and result (success|failure).",
	}, []string{"type", "result"})

	metricUpgradeDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "talos_lifecycle_upgrade_duration_seconds",
		Help:    "Duration of finished upgrades from start to terminal phase.",
		Buckets: []float64{60, 180, 300, 600, 900, 1200, 1800, 2700, 3600},
	}, []string{"type"})

	metricRollbacks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "talos_lifecycle_rollbacks_total",
		Help: "Node rollbacks after failed upgrades, by result (success|failure).",
	}, []string{"result"})

	metricSnapshotFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "talos_lifecycle_etcd_snapshot_failures_total",
		Help: "Failed etcd snapshot attempts.",
	})

	metricSnapshotLastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "talos_lifecycle_etcd_snapshot_last_success_timestamp_seconds",
		Help: "Unix timestamp of the last successfully stored etcd snapshot.",
	})

	registerUpgradeMetricsOnce sync.Once
)

func registerUpgradeMetrics() {
	registerUpgradeMetricsOnce.Do(func() {
		metrics.Registry.MustRegister(
			metricPlanFrozen,
			metricPlanBlocked,
			metricNodes,
			metricUpgradesTotal,
			metricUpgradeDuration,
			metricRollbacks,
			metricSnapshotFailures,
			metricSnapshotLastSuccess,
		)
	})
}

var knownPhases = []upgradev1alpha1.NodePhase{
	upgradev1alpha1.NodePhasePending,
	upgradev1alpha1.NodePhaseUpgrading,
	upgradev1alpha1.NodePhaseVerifying,
	upgradev1alpha1.NodePhaseHealthChecking,
	upgradev1alpha1.NodePhaseSucceeded,
	upgradev1alpha1.NodePhaseRollingBack,
	upgradev1alpha1.NodePhaseFailed,
}

func observeUpgradeResult(upgradeType, result string, startedAt time.Time) {
	metricUpgradesTotal.WithLabelValues(upgradeType, result).Inc()

	if !startedAt.IsZero() {
		metricUpgradeDuration.WithLabelValues(upgradeType).Observe(time.Since(startedAt).Seconds())
	}
}

func updateNodeMetrics(plan *upgradev1alpha1.UpgradePlan) {
	counts := make(map[upgradev1alpha1.NodePhase]int, len(knownPhases))
	for _, n := range plan.Status.Nodes {
		counts[n.Phase]++
	}

	for _, phase := range knownPhases {
		metricNodes.WithLabelValues(string(phase)).Set(float64(counts[phase]))
	}
}
