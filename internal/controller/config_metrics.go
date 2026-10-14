package controller

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

var (
	metricConfigFrozen = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "talos_lifecycle_config_plan_frozen",
		Help: "1 while the machine config plan is frozen, 0 otherwise.",
	})
	metricConfigNodes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_lifecycle_config_nodes",
		Help: "Nodes per machine config phase.",
	}, []string{"phase"})
	metricConfigSkipped = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "talos_lifecycle_config_skipped_nodes",
		Help: "Nodes with reboot-only fields left at the node's value.",
	})
	metricConfigApplies = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "talos_lifecycle_config_applies_total",
		Help: "Finished config applies by result (committed|failed).",
	}, []string{"result"})

	registerConfigMetricsOnce sync.Once
)

var configPhases = []upgradev1alpha1.ConfigNodePhase{
	upgradev1alpha1.ConfigNodePending, upgradev1alpha1.ConfigNodeTrying, upgradev1alpha1.ConfigNodeProbing,
	upgradev1alpha1.ConfigNodeHealthChecking, upgradev1alpha1.ConfigNodeCommitting, upgradev1alpha1.ConfigNodeSucceeded,
	upgradev1alpha1.ConfigNodeReverted, upgradev1alpha1.ConfigNodeFailed,
}

func registerConfigMetrics() {
	registerConfigMetricsOnce.Do(func() {
		metrics.Registry.MustRegister(metricConfigFrozen, metricConfigNodes, metricConfigSkipped, metricConfigApplies)
	})
}

func updateConfigMetrics(plan *upgradev1alpha1.MachineConfigPlan) {
	frozen := 0.0
	if plan.Status.Phase == upgradev1alpha1.ConfigPlanFrozen {
		frozen = 1
	}

	metricConfigFrozen.Set(frozen)

	counts := map[upgradev1alpha1.ConfigNodePhase]int{}
	skipped := 0

	for _, n := range plan.Status.Nodes {
		counts[n.Phase]++

		if len(n.Skipped) > 0 {
			skipped++
		}
	}

	for _, phase := range configPhases {
		metricConfigNodes.WithLabelValues(string(phase)).Set(float64(counts[phase]))
	}

	metricConfigSkipped.Set(float64(skipped))
}
