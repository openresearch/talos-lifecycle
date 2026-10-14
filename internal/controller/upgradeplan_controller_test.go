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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

func newReconciler() *UpgradePlanReconciler {
	return &UpgradePlanReconciler{Client: k8sClient, Scheme: scheme.Scheme}
}

func clusterRequest() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: upgradev1alpha1.UpgradePlanName}}
}

func createClusterPlan(t *testing.T) *upgradev1alpha1.UpgradePlan {
	t.Helper()

	plan := &upgradev1alpha1.UpgradePlan{
		ObjectMeta: metav1.ObjectMeta{Name: upgradev1alpha1.UpgradePlanName},
	}
	if err := k8sClient.Create(testCtx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, plan) })

	return plan
}

func getClusterPlan(t *testing.T) *upgradev1alpha1.UpgradePlan {
	t.Helper()

	var plan upgradev1alpha1.UpgradePlan
	if err := k8sClient.Get(testCtx, clusterRequest().NamespacedName, &plan); err != nil {
		t.Fatalf("get plan: %v", err)
	}

	return &plan
}

func TestReconcileIgnoresWrongName(t *testing.T) {
	requireEnvtest(t)

	result, err := newReconciler().Reconcile(testCtx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "something-else"},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result != (ctrl.Result{}) {
		t.Errorf("result = %+v, want empty", result)
	}
}

func TestReconcileMissingPlanIsNoOp(t *testing.T) {
	requireEnvtest(t)

	result, err := newReconciler().Reconcile(testCtx, clusterRequest())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result != (ctrl.Result{}) {
		t.Errorf("result = %+v, want empty", result)
	}
}

func TestReconcileUnfreezesFailedNode(t *testing.T) {
	requireEnvtest(t)
	plan := createClusterPlan(t)

	plan.Status.Nodes = []upgradev1alpha1.NodeStatus{{
		Name:    "worker-1",
		Address: "10.0.0.10",
		Role:    upgradev1alpha1.RoleWorker,
		Phase:   upgradev1alpha1.NodePhaseFailed,
		Message: "upgrade timed out",
	}}
	if err := k8sClient.Status().Update(testCtx, plan); err != nil {
		t.Fatalf("status update: %v", err)
	}

	plan.Annotations = map[string]string{upgradev1alpha1.UnfreezeAnnotation: "worker-1"}
	if err := k8sClient.Update(testCtx, plan); err != nil {
		t.Fatalf("annotate: %v", err)
	}

	if _, err := newReconciler().Reconcile(testCtx, clusterRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	updated := getClusterPlan(t)
	if _, ok := updated.Annotations[upgradev1alpha1.UnfreezeAnnotation]; ok {
		t.Error("unfreeze annotation was not removed")
	}

	if updated.Status.Nodes[0].Phase != upgradev1alpha1.NodePhasePending {
		t.Errorf("node phase = %s, want Pending", updated.Status.Nodes[0].Phase)
	}
}

func TestReconcileNoMatchUnfreezeIsConsumed(t *testing.T) {
	requireEnvtest(t)
	plan := createClusterPlan(t)

	plan.Status.Nodes = []upgradev1alpha1.NodeStatus{{
		Name:    "worker-1",
		Address: "10.0.0.10",
		Role:    upgradev1alpha1.RoleWorker,
		Phase:   upgradev1alpha1.NodePhaseFailed,
		Message: "upgrade timed out",
	}}
	if err := k8sClient.Status().Update(testCtx, plan); err != nil {
		t.Fatalf("status update: %v", err)
	}

	plan.Annotations = map[string]string{upgradev1alpha1.UnfreezeAnnotation: "wroker-1"}
	if err := k8sClient.Update(testCtx, plan); err != nil {
		t.Fatalf("annotate: %v", err)
	}

	if _, err := newReconciler().Reconcile(testCtx, clusterRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	updated := getClusterPlan(t)
	if _, ok := updated.Annotations[upgradev1alpha1.UnfreezeAnnotation]; ok {
		t.Error("annotation should be consumed even on a no-match")
	}

	if updated.Status.Nodes[0].Phase != upgradev1alpha1.NodePhaseFailed {
		t.Errorf("node phase = %s, want still Failed", updated.Status.Nodes[0].Phase)
	}
}

func TestReconcileSkipsNoOpStatusWrite(t *testing.T) {
	requireEnvtest(t)
	createClusterPlan(t)

	r := newReconciler()

	if _, err := r.Reconcile(testCtx, clusterRequest()); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	first := getClusterPlan(t).ResourceVersion

	if _, err := r.Reconcile(testCtx, clusterRequest()); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	second := getClusterPlan(t).ResourceVersion

	if first != second {
		t.Errorf("resourceVersion changed on a no-op reconcile: %s -> %s", first, second)
	}
}

func TestReconcileEmptySpecIsIdle(t *testing.T) {
	requireEnvtest(t)
	createClusterPlan(t)

	result, err := newReconciler().Reconcile(testCtx, clusterRequest())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != idleInterval {
		t.Errorf("requeue = %s, want %s", result.RequeueAfter, idleInterval)
	}

	updated := getClusterPlan(t)
	if updated.Status.Phase != upgradev1alpha1.PlanPhaseIdle {
		t.Errorf("phase = %s, want Idle", updated.Status.Phase)
	}

	if updated.Status.Reason != "UpToDate" {
		t.Errorf("reason = %s, want UpToDate", updated.Status.Reason)
	}
}

func TestPersistStatusKeepsNodePointers(t *testing.T) {
	plan := createClusterPlan(t)
	plan.Status.Nodes = []upgradev1alpha1.NodeStatus{{Name: "a", Address: "10.0.0.1", Role: upgradev1alpha1.RoleWorker}}

	if err := persistStatus(testCtx, k8sClient, plan); err != nil {
		t.Fatalf("persistStatus: %v", err)
	}

	node := &plan.Status.Nodes[0]
	rv := plan.ResourceVersion
	node.Message = "changed"

	if err := persistStatus(testCtx, k8sClient, plan); err != nil {
		t.Fatalf("persistStatus: %v", err)
	}

	if node != &plan.Status.Nodes[0] {
		t.Fatal("persistStatus replaced status.nodes; the caller's node pointer is orphaned")
	}

	if plan.ResourceVersion == rv {
		t.Error("resourceVersion was not carried back; the next write would conflict")
	}
}

func TestPersistOnFreshSurvivesAConcurrentWrite(t *testing.T) {
	plan := createClusterPlan(t)
	plan.Status.Nodes = []upgradev1alpha1.NodeStatus{{Name: "cp-0", Address: "10.0.0.1", Role: upgradev1alpha1.RoleControlPlane,
		Phase: upgradev1alpha1.NodePhaseUpgrading}}

	if err := persistStatus(testCtx, k8sClient, plan); err != nil {
		t.Fatalf("persistStatus: %v", err)
	}

	other := getClusterPlan(t)
	other.Spec.TalosVersion = talos1142

	if err := k8sClient.Update(testCtx, other); err != nil {
		t.Fatalf("concurrent update: %v", err)
	}

	r := newReconciler()

	err := r.persistOnFresh(testCtx, plan, func(fresh *upgradev1alpha1.UpgradePlan) {
		transition(&fresh.Status.Nodes[0], upgradev1alpha1.NodePhasePending, "upgrade call failed")
	})
	if err != nil {
		t.Fatalf("persistOnFresh: %v", err)
	}

	if got := getClusterPlan(t).Status.Nodes[0].Phase; got != upgradev1alpha1.NodePhasePending {
		t.Errorf("phase = %q, want Pending despite the stale resourceVersion", got)
	}
}
