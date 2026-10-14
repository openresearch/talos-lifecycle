package controller

import (
	"testing"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

func TestMaxSupportedTalosMinor(t *testing.T) {
	max := maxSupportedTalosMinor()
	if max == "" {
		t.Fatal("probe returned no supported minor")
	}

	if !talosTargetSupported(max + ".0") {
		t.Errorf("max minor %s reports itself unsupported", max)
	}
}

func TestTalosTargetSupported(t *testing.T) {
	max := maxSupportedTalosMinor()

	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"v1.12.3", true},
		{"v1.13.10", true},
		{max + ".0", true},
		{"v1.99.0", false},
		{"", true},
	} {
		if got := talosTargetSupported(tc.version); got != tc.want {
			t.Errorf("talosTargetSupported(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

func TestNextTalosNodeSkipsUnsupportedTarget(t *testing.T) {
	plan := &upgradev1alpha1.UpgradePlan{
		Spec: upgradev1alpha1.UpgradePlanSpec{TalosVersion: "v1.99.0"},
		Status: upgradev1alpha1.UpgradePlanStatus{
			Nodes: []upgradev1alpha1.NodeStatus{
				{
					Name:           "control-plane-0",
					Role:           upgradev1alpha1.RoleControlPlane,
					CurrentVersion: "v1.14.0",
					Phase:          upgradev1alpha1.NodePhasePending,
				},
			},
		},
	}

	if node := nextTalosNode(plan); node != nil {
		t.Errorf("nextTalosNode picked %s for unsupported target v1.99.0", node.Name)
	}

	plan.Spec.TalosVersion = maxSupportedTalosMinor() + ".0"
	plan.Status.Nodes[0].CurrentVersion = "v1.12.3"

	if node := nextTalosNode(plan); node == nil {
		t.Error("nextTalosNode skipped a supported target")
	}
}
