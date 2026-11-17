package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Talos skips its own node Ready check on clusters with cni: none, so the fake reports healthy.
func TestClusterHealthNeedsEveryNodeReady(t *testing.T) {
	t.Parallel()

	node := func(name, address string, ready corev1.ConditionStatus, labels map[string]string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			Status: corev1.NodeStatus{
				Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: address}},
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}},
			},
		}
	}
	controlPlane := map[string]string{"node-role.kubernetes.io/control-plane": ""}

	for _, tc := range []struct {
		name   string
		worker corev1.ConditionStatus
		want   bool
	}{
		{"worker Ready", corev1.ConditionTrue, true},
		{"worker Ready Unknown", corev1.ConditionUnknown, false},
		{"worker Ready False", corev1.ConditionFalse, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := fake.NewClientBuilder().WithScheme(upgradeScheme(t)).WithObjects(
				node("cp-0", "10.0.0.1", corev1.ConditionTrue, controlPlane),
				node("worker-1", "10.0.0.11", tc.worker, nil),
			).Build()

			var gotCP, gotWorkers []string

			talos := func(_ context.Context, cp, workers []string) (bool, string, error) {
				gotCP, gotWorkers = cp, workers

				return true, "", nil
			}

			healthy, reason, err := clusterHealth(context.Background(), c, talos)
			if err != nil {
				t.Fatalf("clusterHealth: %v", err)
			}

			if healthy != tc.want {
				t.Fatalf("healthy = %v (%q), want %v", healthy, reason, tc.want)
			}

			if !tc.want && !strings.Contains(reason, "worker-1") {
				t.Errorf("reason = %q, want it to name worker-1", reason)
			}

			if tc.want && (!slices.Equal(gotCP, []string{"10.0.0.1"}) || !slices.Equal(gotWorkers, []string{"10.0.0.11"})) {
				t.Errorf("talos check got control plane %v, workers %v", gotCP, gotWorkers)
			}
		})
	}
}
