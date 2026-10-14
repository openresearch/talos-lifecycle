/*
Copyright 2026 OpenResearch GmbH.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
	"github.com/openresearch/talos-lifecycle/pkg/confignode"
	"github.com/openresearch/talos-lifecycle/pkg/configsource"
)

type clusterFixture struct {
	r     *MachineConfigPlanReconciler
	plan  *upgradev1alpha1.MachineConfigPlan
	nodes map[string]*confignode.FakeNode
}

type nodeSpec struct {
	name    string
	address string
	role    string
}

var testCluster = []nodeSpec{
	{"control-plane-0", "10.0.0.1", upgradev1alpha1.RoleControlPlane},
	{"control-plane-1", "10.0.0.2", upgradev1alpha1.RoleControlPlane},
	{"worker-0", "10.0.0.10", upgradev1alpha1.RoleWorker},
	{"worker-1", "10.0.0.11", upgradev1alpha1.RoleWorker},
}

const baseHostname = "old-name"

func newClusterFixture(t *testing.T, inSource string) *clusterFixture {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := upgradev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}

	plan := &upgradev1alpha1.MachineConfigPlan{
		ObjectMeta: metav1.ObjectMeta{Name: upgradev1alpha1.MachineConfigPlanName},
		Spec: upgradev1alpha1.MachineConfigPlanSpec{
			Mode:            upgradev1alpha1.ModeReconcile,
			CooldownMinutes: ptrInt64(0),
			Verify:          &upgradev1alpha1.ConfigVerifyTimeouts{SoakSeconds: secs(0)},
		},
	}

	objs := []client.Object{plan}
	fakes := map[string]*confignode.FakeNode{}

	for _, spec := range testCluster {
		labels := map[string]string{}
		if spec.role == upgradev1alpha1.RoleControlPlane {
			labels["node-role.kubernetes.io/control-plane"] = ""
		}

		objs = append(objs, &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: spec.name, Labels: labels},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: spec.address},
			}},
		})

		fakes[spec.address] = confignode.NewFakeNode(canonical(t, baseConfig(baseHostname)))
	}

	r := &MachineConfigPlanReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(objs...).
			WithStatusSubresource(&upgradev1alpha1.MachineConfigPlan{}).Build(),
		Scheme: scheme,
		Dialer: &confignode.FakeDialer{Nodes: fakes},
		SourceFor: func(*upgradev1alpha1.MachineConfigPlan) (configsource.Fetcher, error) {
			return staticFetcher{fsys: sourceFS(inSource)}, nil
		},
	}

	return &clusterFixture{r: r, plan: plan, nodes: fakes}
}

func ptrInt64(v int64) *int64 { return &v }

const maxSettlePasses = 400

func (f *clusterFixture) reconcileUntilSettled(t *testing.T) []string {
	t.Helper()

	var order []string

	seen := map[string]bool{}

	for range maxSettlePasses {
		if _, err := f.r.reconcilePlan(context.Background(), f.plan); err != nil {
			t.Fatalf("reconcile: %v", err)
		}

		inFlight := 0

		for i := range f.plan.Status.Nodes {
			n := &f.plan.Status.Nodes[i]
			if n.Phase.InFlight() {
				inFlight++

				if !seen[n.Name] {
					seen[n.Name] = true

					order = append(order, n.Name)
				}
			}
		}

		if inFlight > 1 {
			t.Fatalf("%d nodes in flight at once; the rollout must be sequential", inFlight)
		}

		switch f.plan.Status.Phase {
		case upgradev1alpha1.ConfigPlanIdle, upgradev1alpha1.ConfigPlanFrozen:
			return order
		case upgradev1alpha1.ConfigPlanWaiting:
			if f.plan.Status.Reason != reasonConfigClusterUnready {
				return order
			}
		}
	}

	return order
}

func (f *clusterFixture) reconcileViaAPIUntilSettled(t *testing.T) {
	t.Helper()

	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: upgradev1alpha1.MachineConfigPlanName}}

	for range maxSettlePasses {
		if _, err := f.r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}

		var plan upgradev1alpha1.MachineConfigPlan
		if err := f.r.Get(context.Background(), req.NamespacedName, &plan); err != nil {
			t.Fatalf("get plan: %v", err)
		}

		if plan.Status.Phase == upgradev1alpha1.ConfigPlanIdle ||
			plan.Status.Phase == upgradev1alpha1.ConfigPlanFrozen {
			return
		}
	}
}

func TestRolloutConvergesSequentially(t *testing.T) {
	t.Parallel()

	f := newClusterFixture(t, "new-name")

	order := f.reconcileUntilSettled(t)

	if f.plan.Status.Phase != upgradev1alpha1.ConfigPlanIdle {
		t.Fatalf("plan phase = %q (%s), want Idle", f.plan.Status.Phase, f.plan.Status.Message)
	}

	for i := range f.plan.Status.Nodes {
		n := &f.plan.Status.Nodes[i]
		if n.Phase != upgradev1alpha1.ConfigNodeSucceeded {
			t.Errorf("node %s ended in %q: %s", n.Name, n.Phase, n.Message)
		}
	}

	if len(order) != len(testCluster) {
		t.Fatalf("rolled out %d nodes, want %d (order: %v)", len(order), len(testCluster), order)
	}

	if !strings.HasPrefix(order[0], "worker-") || !strings.HasPrefix(order[1], "worker-") {
		t.Errorf("workers must roll out first, got order %v", order)
	}

	for _, node := range f.nodes {
		if node.TryPending() {
			t.Error("a node was left with an armed try timer after the rollout settled")
		}
	}
}

func TestRolloutPersistsOnEveryNode(t *testing.T) {
	t.Parallel()

	f := newClusterFixture(t, "new-name")
	f.reconcileUntilSettled(t)

	want := string(canonical(t, baseConfig("new-name")))

	for addr, node := range f.nodes {
		if got := string(node.SlotBytes(confignode.SlotPersistent)); got != want {
			t.Errorf("node %s did not persist the new config", addr)
		}
	}
}

func TestOneBadNodeFreezesTheRollout(t *testing.T) {
	t.Parallel()

	f := newClusterFixture(t, "new-name")

	f.nodes["10.0.0.10"].RejectApply = true

	f.reconcileUntilSettled(t)

	if f.plan.Status.Phase != upgradev1alpha1.ConfigPlanFrozen {
		t.Fatalf("plan phase = %q, want Frozen", f.plan.Status.Phase)
	}

	untouched := string(canonical(t, baseConfig(baseHostname)))

	for addr, node := range f.nodes {
		if addr == "10.0.0.10" {
			continue
		}

		if got := string(node.SlotBytes(confignode.SlotPersistent)); got != untouched {
			t.Errorf("node %s was modified while the plan was frozen", addr)
		}
	}
}

func TestCooldownGatesTheNextNode(t *testing.T) {
	t.Parallel()

	f := newClusterFixture(t, "new-name")
	f.plan.Spec.CooldownMinutes = ptrInt64(30)

	for range 50 {
		if _, err := f.r.reconcilePlan(context.Background(), f.plan); err != nil {
			t.Fatalf("reconcile: %v", err)
		}

		done := 0

		for i := range f.plan.Status.Nodes {
			if f.plan.Status.Nodes[i].Phase == upgradev1alpha1.ConfigNodeSucceeded {
				done++
			}
		}

		if done > 0 {
			break
		}
	}

	if _, err := f.r.reconcilePlan(context.Background(), f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if f.plan.Status.Phase != upgradev1alpha1.ConfigPlanWaiting {
		t.Fatalf("plan phase = %q, want Waiting", f.plan.Status.Phase)
	}

	if f.plan.Status.Reason != reasonConfigCooldown {
		t.Errorf("reason = %q, want %q", f.plan.Status.Reason, reasonConfigCooldown)
	}

	applied := 0

	for _, node := range f.nodes {
		if string(node.SlotBytes(confignode.SlotPersistent)) == string(canonical(t, baseConfig("new-name"))) {
			applied++
		}
	}

	if applied != 1 {
		t.Errorf("%d nodes applied during the cooldown, want 1", applied)
	}
}

func TestWindowGatesTheRollout(t *testing.T) {
	t.Parallel()

	f := newClusterFixture(t, "new-name")

	closed := time.Now().Add(3 * time.Hour)
	f.plan.Spec.Window = &upgradev1alpha1.UpgradeWindow{
		Start: closed.Format("15:04"),
		End:   closed.Add(time.Minute).Format("15:04"),
	}

	if _, err := f.r.reconcilePlan(context.Background(), f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if f.plan.Status.Phase != upgradev1alpha1.ConfigPlanWaiting {
		t.Fatalf("plan phase = %q, want Waiting", f.plan.Status.Phase)
	}

	if f.plan.Status.Reason != reasonConfigWindow {
		t.Errorf("reason = %q, want %q", f.plan.Status.Reason, reasonConfigWindow)
	}

	for addr, node := range f.nodes {
		for _, call := range node.Calls {
			if strings.HasPrefix(call, "Apply") {
				t.Errorf("node %s was applied to outside the window", addr)
			}
		}
	}
}

func TestUnhealthyClusterBlocksTheStart(t *testing.T) {
	t.Parallel()

	f := newClusterFixture(t, "new-name")
	f.r.HealthCheck = func(context.Context) (bool, string, error) {
		return false, "etcd member down", nil
	}

	if _, err := f.r.reconcilePlan(context.Background(), f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if f.plan.Status.Reason != reasonConfigClusterUnready {
		t.Errorf("reason = %q, want %q", f.plan.Status.Reason, reasonConfigClusterUnready)
	}

	for addr, node := range f.nodes {
		for _, call := range node.Calls {
			if strings.HasPrefix(call, "Apply") {
				t.Errorf("node %s was applied to while the cluster was unhealthy", addr)
			}
		}
	}
}

func TestAppliedRevisionRecordedAfterRollout(t *testing.T) {
	t.Parallel()

	f := newClusterFixture(t, "new-name")
	f.reconcileUntilSettled(t)

	if f.plan.Status.AppliedRevision == "" {
		t.Fatal("appliedRevision must be recorded once every node matches")
	}

	if f.plan.Status.AppliedRevision != f.plan.Status.SourceRevision {
		t.Errorf("appliedRevision %q != sourceRevision %q after a full rollout",
			f.plan.Status.AppliedRevision, f.plan.Status.SourceRevision)
	}
}

func TestAppliedRevisionWaitsForAnUnreadableNode(t *testing.T) {
	t.Parallel()

	f := newClusterFixture(t, "new-name")
	f.reconcileUntilSettled(t)

	first := f.plan.Status.AppliedRevision

	f.nodes["10.0.0.11"].Unreachable = true
	f.r.SourceFor = func(*upgradev1alpha1.MachineConfigPlan) (configsource.Fetcher, error) {
		return staticFetcher{fsys: sourceFS("third-name")}, nil
	}

	for range 50 {
		if _, err := f.r.reconcilePlan(context.Background(), f.plan); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	if got := f.plan.Status.AppliedRevision; got != first {
		t.Errorf("appliedRevision = %q, want %q: worker-1 was never read against %s (%s)",
			got, first, f.plan.Status.SourceRevision, f.plan.Status.Message)
	}

	f.nodes["10.0.0.11"].Unreachable = false
	f.reconcileUntilSettled(t)

	if f.plan.Status.AppliedRevision != f.plan.Status.SourceRevision {
		t.Errorf("appliedRevision %q != sourceRevision %q once worker-1 is back and converged",
			f.plan.Status.AppliedRevision, f.plan.Status.SourceRevision)
	}
}

func TestSecondRevisionRollsOutToo(t *testing.T) {
	t.Parallel()

	f := newClusterFixture(t, "second-name")
	f.reconcileUntilSettled(t)

	if f.plan.Status.Phase != upgradev1alpha1.ConfigPlanIdle {
		t.Fatalf("first rollout did not settle: %q %s", f.plan.Status.Phase, f.plan.Status.Message)
	}

	first := f.plan.Status.AppliedRevision

	f.r.SourceFor = func(*upgradev1alpha1.MachineConfigPlan) (configsource.Fetcher, error) {
		return staticFetcher{fsys: sourceFS("third-name")}, nil
	}

	f.reconcileUntilSettled(t)

	if f.plan.Status.Phase != upgradev1alpha1.ConfigPlanIdle {
		t.Fatalf("second rollout did not settle: %q %s", f.plan.Status.Phase, f.plan.Status.Message)
	}

	if f.plan.Status.AppliedRevision == first {
		t.Error("appliedRevision did not advance for the second revision")
	}

	want := string(canonical(t, baseConfig("third-name")))

	for addr, node := range f.nodes {
		if got := string(node.SlotBytes(confignode.SlotPersistent)); got != want {
			t.Errorf("node %s did not converge on the second revision", addr)
		}
	}
}

func TestUnfreezeResumesTheRollout(t *testing.T) {
	t.Parallel()

	f := newClusterFixture(t, "new-name")
	f.nodes["10.0.0.10"].RejectApply = true

	f.reconcileViaAPIUntilSettled(t)

	ctx := context.Background()

	var live upgradev1alpha1.MachineConfigPlan
	if err := f.r.Get(ctx, client.ObjectKey{Name: upgradev1alpha1.MachineConfigPlanName}, &live); err != nil {
		t.Fatalf("get plan: %v", err)
	}

	if live.Status.Phase != upgradev1alpha1.ConfigPlanFrozen {
		t.Fatalf("expected the plan to freeze, got %q", live.Status.Phase)
	}

	f.nodes["10.0.0.10"].RejectApply = false

	live.Annotations = map[string]string{upgradev1alpha1.UnfreezeAnnotation: "all"}
	if err := f.r.Update(ctx, &live); err != nil {
		t.Fatalf("annotate plan: %v", err)
	}

	f.reconcileViaAPIUntilSettled(t)

	if err := f.r.Get(ctx, client.ObjectKey{Name: upgradev1alpha1.MachineConfigPlanName}, &live); err != nil {
		t.Fatalf("get plan: %v", err)
	}

	if live.Status.Phase != upgradev1alpha1.ConfigPlanIdle {
		t.Fatalf("plan phase = %q (%s), want Idle after unfreeze",
			live.Status.Phase, live.Status.Message)
	}

	want := string(canonical(t, baseConfig("new-name")))

	for addr, node := range f.nodes {
		if got := string(node.SlotBytes(confignode.SlotPersistent)); got != want {
			t.Errorf("node %s did not converge after the unfreeze", addr)
		}
	}
}

func TestClusterFixtureStartsDivergent(t *testing.T) {
	t.Parallel()

	f := newClusterFixture(t, "new-name")

	if _, err := f.r.reconcilePlan(context.Background(), f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(f.plan.Status.Nodes) != len(testCluster) {
		t.Fatalf("observed %d nodes, want %d", len(f.plan.Status.Nodes), len(testCluster))
	}

	pendingOrInFlight := 0

	for i := range f.plan.Status.Nodes {
		n := &f.plan.Status.Nodes[i]
		if n.Phase == upgradev1alpha1.ConfigNodePending || n.Phase.InFlight() {
			pendingOrInFlight++
		}
	}

	if pendingOrInFlight != len(testCluster) {
		t.Errorf("%d of %d nodes need work; the fixture should start fully divergent",
			pendingOrInFlight, len(testCluster))
	}
}

func TestUnfreezePersistsInOnePass(t *testing.T) {
	t.Parallel()

	f := newClusterFixture(t, "new-name")
	f.nodes["10.0.0.10"].RejectApply = true
	f.reconcileViaAPIUntilSettled(t)

	ctx := context.Background()
	key := client.ObjectKey{Name: upgradev1alpha1.MachineConfigPlanName}

	var live upgradev1alpha1.MachineConfigPlan
	if err := f.r.Get(ctx, key, &live); err != nil {
		t.Fatalf("get plan: %v", err)
	}

	if live.Status.Phase != upgradev1alpha1.ConfigPlanFrozen {
		t.Fatalf("setup: plan phase = %q, want Frozen", live.Status.Phase)
	}

	live.Annotations = map[string]string{upgradev1alpha1.UnfreezeAnnotation: "worker-0"}
	if err := f.r.Update(ctx, &live); err != nil {
		t.Fatalf("annotate: %v", err)
	}

	if _, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if err := f.r.Get(ctx, key, &live); err != nil {
		t.Fatalf("get plan: %v", err)
	}

	if _, still := live.Annotations[upgradev1alpha1.UnfreezeAnnotation]; still {
		t.Error("annotation should be consumed")
	}

	for _, n := range live.Status.Nodes {
		if n.Name == "worker-0" && n.Phase == upgradev1alpha1.ConfigNodeFailed {
			t.Fatal("annotation consumed but the reset was not persisted")
		}
	}
}
