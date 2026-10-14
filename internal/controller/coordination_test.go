package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

func upgradePlanWith(phase upgradev1alpha1.PlanPhase, node upgradev1alpha1.NodePhase) *upgradev1alpha1.UpgradePlan {
	return &upgradev1alpha1.UpgradePlan{
		ObjectMeta: metav1.ObjectMeta{Name: upgradev1alpha1.UpgradePlanName},
		Status: upgradev1alpha1.UpgradePlanStatus{
			Phase: phase,
			Nodes: []upgradev1alpha1.NodeStatus{{Name: "control-plane-0", Phase: node}},
		},
	}
}

func TestConfigLoopYieldsToUpgrade(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	up := upgradePlanWith(upgradev1alpha1.PlanPhaseUpgrading, upgradev1alpha1.NodePhaseUpgrading)
	if err := f.r.Create(ctx, up); err != nil {
		t.Fatalf("create UpgradePlan: %v", err)
	}

	if _, err := f.r.reconcilePlan(ctx, f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, call := range f.node.Calls {
		if strings.HasPrefix(call, "Apply") {
			t.Fatalf("applied %q while the upgrade loop was mid-node", call)
		}
	}

	if f.plan.Status.Reason != reasonConfigUpgradeInFlight {
		t.Fatalf("reason = %q, want %q", f.plan.Status.Reason, reasonConfigUpgradeInFlight)
	}

	up.Status = upgradePlanWith(upgradev1alpha1.PlanPhaseIdle, upgradev1alpha1.NodePhaseSucceeded).Status
	if err := f.r.Update(ctx, up); err != nil {
		t.Fatalf("update UpgradePlan: %v", err)
	}

	if _, err := f.r.reconcilePlan(ctx, f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if f.nodeStatus().Phase != upgradev1alpha1.ConfigNodeProbing {
		t.Errorf("phase = %q, want Probing once the upgrade loop is idle", f.nodeStatus().Phase)
	}
}

func TestUpgradeInFlight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		plan *upgradev1alpha1.UpgradePlan
		want string
	}{
		{"no plan", nil, ""},
		{"idle", upgradePlanWith(upgradev1alpha1.PlanPhaseIdle, upgradev1alpha1.NodePhaseSucceeded), ""},
		{"node in flight", upgradePlanWith(upgradev1alpha1.PlanPhaseUpgrading, upgradev1alpha1.NodePhaseVerifying),
			"node control-plane-0"},
		{"kubernetes rollout", upgradePlanWith(upgradev1alpha1.PlanPhaseUpgrading, upgradev1alpha1.NodePhaseSucceeded),
			"the Kubernetes upgrade"},
		{"frozen", upgradePlanWith(upgradev1alpha1.PlanPhaseFrozen, upgradev1alpha1.NodePhaseFailed), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := fake.NewClientBuilder().WithScheme(coordinationScheme(t))
			if tt.plan != nil {
				b = b.WithObjects(tt.plan)
			}

			got, err := upgradeInFlight(context.Background(), b.Build())
			if err != nil {
				t.Fatalf("upgradeInFlight: %v", err)
			}

			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfigInFlight(t *testing.T) {
	t.Parallel()

	plan := func(phase upgradev1alpha1.ConfigNodePhase) *upgradev1alpha1.MachineConfigPlan {
		return &upgradev1alpha1.MachineConfigPlan{
			ObjectMeta: metav1.ObjectMeta{Name: upgradev1alpha1.MachineConfigPlanName},
			Status: upgradev1alpha1.MachineConfigPlanStatus{
				Nodes: []upgradev1alpha1.ConfigNodeStatus{{Name: testWorkerName, Phase: phase}},
			},
		}
	}

	tests := []struct {
		name string
		plan *upgradev1alpha1.MachineConfigPlan
		want string
	}{
		{"no plan", nil, ""},
		{"pending", plan(upgradev1alpha1.ConfigNodePending), ""},
		{"failed", plan(upgradev1alpha1.ConfigNodeFailed), ""},
		{"trying", plan(upgradev1alpha1.ConfigNodeTrying), testWorkerName},
		{"reverted", plan(upgradev1alpha1.ConfigNodeReverted), testWorkerName},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := fake.NewClientBuilder().WithScheme(coordinationScheme(t))
			if tt.plan != nil {
				b = b.WithObjects(tt.plan)
			}

			got, err := configInFlight(context.Background(), b.Build())
			if err != nil {
				t.Fatalf("configInFlight: %v", err)
			}

			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfigLoopRechecksUpgradeBeforeTry(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	f.r.HealthCheck = func(ctx context.Context) (bool, string, error) {
		up := upgradePlanWith(upgradev1alpha1.PlanPhaseUpgrading, upgradev1alpha1.NodePhaseUpgrading)
		if err := f.r.Create(ctx, up); err != nil {
			t.Errorf("create UpgradePlan: %v", err)
		}

		return true, "", nil
	}

	if _, err := f.r.reconcilePlan(ctx, f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, call := range f.node.Calls {
		if strings.HasPrefix(call, "Apply") {
			t.Fatalf("applied %q after the upgrade loop started a node", call)
		}
	}

	if got := f.nodeStatus().Phase; got != upgradev1alpha1.ConfigNodePending {
		t.Errorf("phase = %q, want Pending", got)
	}
}

func TestUpgradeLoopRechecksConfigBeforeIntent(t *testing.T) {
	t.Parallel()

	up := upgradePlanWith(upgradev1alpha1.PlanPhaseWaiting, upgradev1alpha1.NodePhasePending)
	up.Spec.TalosVersion = talos1142
	up.Spec.InstallerImageBase = "example.com/installer"
	up.Status.Nodes[0].CurrentVersion = "v1.14.0"

	mcp := &upgradev1alpha1.MachineConfigPlan{
		ObjectMeta: metav1.ObjectMeta{Name: upgradev1alpha1.MachineConfigPlanName},
		Status: upgradev1alpha1.MachineConfigPlanStatus{
			Nodes: []upgradev1alpha1.ConfigNodeStatus{{Name: testWorkerName, Phase: upgradev1alpha1.ConfigNodeTrying}},
		},
	}

	c := fake.NewClientBuilder().WithScheme(coordinationScheme(t)).
		WithObjects(up, mcp).
		WithStatusSubresource(&upgradev1alpha1.UpgradePlan{}, &upgradev1alpha1.MachineConfigPlan{}).Build()
	r := &UpgradePlanReconciler{Client: c}
	ctx := context.Background()

	var plan upgradev1alpha1.UpgradePlan
	if err := c.Get(ctx, client.ObjectKeyFromObject(up), &plan); err != nil {
		t.Fatalf("get UpgradePlan: %v", err)
	}

	if _, err := r.startNodeUpgrade(ctx, &plan, &plan.Status.Nodes[0]); err != nil {
		t.Logf("startNodeUpgrade: %v", err)
	}

	var stored upgradev1alpha1.UpgradePlan
	if err := c.Get(ctx, client.ObjectKeyFromObject(up), &stored); err != nil {
		t.Fatalf("get UpgradePlan: %v", err)
	}

	if got := stored.Status.Nodes[0].Phase; got != upgradev1alpha1.NodePhasePending {
		t.Errorf("stored phase = %q, want Pending while the config loop has a node in flight", got)
	}

	if plan.Status.Reason != reasonWaitingForConfig {
		t.Errorf("reason = %q, want %q", plan.Status.Reason, reasonWaitingForConfig)
	}
}

func coordinationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	if err := upgradev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	return s
}
