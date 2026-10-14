package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

func TestGateResult(t *testing.T) {
	if !(gateResult{}).passed() {
		t.Error("zero gateResult must pass")
	}

	g := blocked("node %s is not ready", "cp-0")
	if g.passed() {
		t.Error("blocked gate must not pass")
	}

	if g.reason != "node cp-0 is not ready" {
		t.Errorf("reason = %q", g.reason)
	}
}

func TestControlPlaneNames(t *testing.T) {
	plan := &upgradev1alpha1.UpgradePlan{
		Status: upgradev1alpha1.UpgradePlanStatus{
			Nodes: []upgradev1alpha1.NodeStatus{
				{Name: "cp-0", Role: upgradev1alpha1.RoleControlPlane},
				{Name: "worker-0", Role: upgradev1alpha1.RoleWorker},
				{Name: "cp-1", Role: upgradev1alpha1.RoleControlPlane},
			},
		},
	}

	got := controlPlaneNames(plan)
	if len(got) != 2 || got[0] != "cp-0" || got[1] != "cp-1" {
		t.Errorf("controlPlaneNames = %v, want [cp-0 cp-1]", got)
	}

	if names := controlPlaneNames(&upgradev1alpha1.UpgradePlan{}); names != nil {
		t.Errorf("empty plan returned %v", names)
	}
}

func TestIsPodReady(t *testing.T) {
	ready := func(phase corev1.PodPhase, status corev1.ConditionStatus) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "kube-apiserver-cp-0"},
			Status: corev1.PodStatus{
				Phase:      phase,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
			},
		}
	}

	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"running and ready", ready(corev1.PodRunning, corev1.ConditionTrue), true},
		{"running not ready", ready(corev1.PodRunning, corev1.ConditionFalse), false},
		{"pending", ready(corev1.PodPending, corev1.ConditionTrue), false},
		{"succeeded", ready(corev1.PodSucceeded, corev1.ConditionTrue), false},
		{"no conditions", &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPodReady(tc.pod); got != tc.want {
				t.Errorf("isPodReady = %v, want %v", got, tc.want)
			}
		})
	}
}
