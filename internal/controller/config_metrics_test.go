package controller

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

func TestUpdateConfigMetrics(t *testing.T) {
	plan := &upgradev1alpha1.MachineConfigPlan{Status: upgradev1alpha1.MachineConfigPlanStatus{
		Phase: upgradev1alpha1.ConfigPlanFrozen,
		Nodes: []upgradev1alpha1.ConfigNodeStatus{
			{Name: "a", Phase: upgradev1alpha1.ConfigNodeFailed},
			{Name: "b", Phase: upgradev1alpha1.ConfigNodeSucceeded, Skipped: []string{"v1alpha1: machine.env"}},
		},
	}}

	updateConfigMetrics(plan)

	for name, tc := range map[string]struct{ got, want float64 }{
		"frozen":    {testutil.ToFloat64(metricConfigFrozen), 1},
		"failed":    {testutil.ToFloat64(metricConfigNodes.WithLabelValues("Failed")), 1},
		"succeeded": {testutil.ToFloat64(metricConfigNodes.WithLabelValues("Succeeded")), 1},
		"pending":   {testutil.ToFloat64(metricConfigNodes.WithLabelValues("Pending")), 0},
		"skipped":   {testutil.ToFloat64(metricConfigSkipped), 1},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", name, tc.got, tc.want)
		}
	}
}
