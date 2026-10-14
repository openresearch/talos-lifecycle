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
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
	"github.com/openresearch/talos-lifecycle/pkg/confignode"
	"github.com/openresearch/talos-lifecycle/pkg/configsource"
)

const (
	testWorkerAddr = "10.0.0.10"
	testWorkerName = "worker-0"
)

// Talos validation requires CA material on workers.
const testCA = "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUJQekNCOHFBREFnRUNBaEVBL2hiaWhnanFoT2dmL0VRZmlJcjE4REFGQmdNclpYQXdFREVPTUF3R0ExVUUKQ2hNRmRHRnNiM013SGhjTk1qWXdPVEUxTVRjME1ESTFXaGNOTXpZd09URXlNVGMwTURJMVdqQVFNUTR3REFZRApWUVFLRXdWMFlXeHZjekFxTUFVR0F5dGxjQU1oQUJBaC9SbU5ucmF5Z1NCeWl2K1dubTVqem9mOHNwdjR1ZlhLCjJINnNLWllFbzJFd1h6QU9CZ05WSFE4QkFmOEVCQU1DQW9Rd0hRWURWUjBsQkJZd0ZBWUlLd1lCQlFVSEF3RUcKQ0NzR0FRVUZCd01DTUE4R0ExVWRFd0VCL3dRRk1BTUJBZjh3SFFZRFZSME9CQllFRk8xeFBBbjQ4UUlzakcrYgpUY21MZzNqUW55Y3JNQVVHQXl0bGNBTkJBSkwyT2s2cGpKdnJzd3ZabGM5dW1QNXBORkhza2srSEtBSGM3RGUwCkx4UDVVd0ZvV1JDMVAyT1pteUNkQTZDbHVMclE3WlA0dmlROTBaem8weXRSU1FJPQotLS0tLUVORCBDRVJUSUZJQ0FURS0tLS0tCg=="

func baseConfig(hostname string) string {
	return fmt.Sprintf(`version: v1alpha1
debug: false
machine:
    type: worker
    token: aaaaaa.bbbbbbbbbbbbbbbb
    ca:
        crt: %s
    network:
        hostname: %s
cluster:
    id: test
    secret: c2VjcmV0
    ca:
        crt: %s
    controlPlane:
        endpoint: https://10.0.0.1:6443
`, testCA, hostname, testCA)
}

func canonical(t *testing.T, raw string) []byte {
	t.Helper()

	p, err := confignode.Load([]byte(raw))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	b, err := confignode.Encode(p)
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}

	return b
}

func sourceFS(hostname string) fstest.MapFS {
	return fstest.MapFS{
		"base/00-v1alpha1.yaml": &fstest.MapFile{Data: []byte(baseConfig(hostname))},
	}
}

type driveFixture struct {
	r    *MachineConfigPlanReconciler
	plan *upgradev1alpha1.MachineConfigPlan
	node *confignode.FakeNode
}

func newDriveFixture(t *testing.T, onNode, inSource string) *driveFixture {
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
			Mode:   upgradev1alpha1.ModeReconcile,
			Verify: &upgradev1alpha1.ConfigVerifyTimeouts{SoakSeconds: secs(0)},
		},
		Status: upgradev1alpha1.MachineConfigPlanStatus{
			Nodes: []upgradev1alpha1.ConfigNodeStatus{{
				Name:    testWorkerName,
				Address: testWorkerAddr,
				Role:    upgradev1alpha1.RoleWorker,
				Phase:   upgradev1alpha1.ConfigNodePending,
			}},
		},
	}

	node := confignode.NewFakeNode(canonical(t, baseConfig(onNode)))

	fsys := sourceFS(inSource)

	k8sNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: testWorkerName},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: testWorkerAddr},
		}},
	}

	r := &MachineConfigPlanReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(plan, k8sNode).
			WithStatusSubresource(&upgradev1alpha1.MachineConfigPlan{}).Build(),
		Scheme: scheme,
		Dialer: &confignode.FakeDialer{Nodes: map[string]*confignode.FakeNode{testWorkerAddr: node}},
		SourceFor: func(*upgradev1alpha1.MachineConfigPlan) (configsource.Fetcher, error) {
			return staticFetcher{fsys: fsys}, nil
		},
	}

	return &driveFixture{r: r, plan: plan, node: node}
}

type staticFetcher struct {
	fsys fstest.MapFS
	err  error
}

func (s staticFetcher) Fetch(context.Context) (*configsource.Source, error) {
	if s.err != nil {
		return nil, s.err
	}

	rev, err := configsource.RevisionOf(s.fsys)
	if err != nil {
		return nil, err
	}

	return &configsource.Source{FS: s.fsys, Revision: rev}, nil
}

func (f *driveFixture) source(t *testing.T) *configsource.Source {
	t.Helper()

	src, err := f.r.fetchSource(context.Background(), f.plan)
	if err != nil {
		t.Fatalf("fetch source: %v", err)
	}

	return src
}

func failedNode(p *upgradev1alpha1.MachineConfigPlan) *upgradev1alpha1.ConfigNodeStatus {
	return findNode(p, func(n *upgradev1alpha1.ConfigNodeStatus) bool { return n.Phase == upgradev1alpha1.ConfigNodeFailed })
}

func (f *driveFixture) nodeStatus() *upgradev1alpha1.ConfigNodeStatus {
	return &f.plan.Status.Nodes[0]
}

func (f *driveFixture) step(t *testing.T) ctrl.Result {
	t.Helper()

	res, err := f.r.driveConfigNode(context.Background(), f.plan, f.nodeStatus(), f.source(t))
	if err != nil {
		t.Fatalf("drive: %v", err)
	}

	return res
}

func TestHappyPathCommitsOnlyAfterVerification(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()
	if node.Phase != upgradev1alpha1.ConfigNodeProbing {
		t.Fatalf("after try, phase = %q, want Probing", node.Phase)
	}

	if !bytes.Equal(f.node.SlotBytes(confignode.SlotPersistent), canonical(t, baseConfig("old-name"))) {
		t.Fatal("TRY must not write the persistent slot")
	}

	if f.step(t); node.Phase != upgradev1alpha1.ConfigNodeHealthChecking {
		t.Fatalf("after probe, phase = %q, want HealthChecking", node.Phase)
	}

	if f.step(t); node.Phase != upgradev1alpha1.ConfigNodeCommitting {
		t.Fatalf("after health, phase = %q, want Committing", node.Phase)
	}

	if f.step(t); node.Phase != upgradev1alpha1.ConfigNodeSucceeded {
		t.Fatalf("after commit, phase = %q, want Succeeded (message: %s)", node.Phase, node.Message)
	}

	if !bytes.Equal(f.node.SlotBytes(confignode.SlotPersistent), canonical(t, baseConfig("new-name"))) {
		t.Error("commit did not persist the verified config")
	}

	if f.node.TryPending() {
		t.Error("commit must cancel the try timer")
	}

	if node.TryDeadline != nil || node.TryIssuedAt != nil {
		t.Error("try bookkeeping must be cleared once the node succeeds")
	}
}

func TestUnreachableNodeRevertsWithoutControllerAction(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	f.plan.Spec.Verify = &upgradev1alpha1.ConfigVerifyTimeouts{
		SoakSeconds:          secs(0),
		TryTimeoutSeconds:    secs(60),
		ProbeTimeoutSeconds:  secs(10),
		HealthTimeoutSeconds: secs(10),
	}

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()

	f.node.Unreachable = true

	node.LastTransition = metav1.NewTime(time.Now().Add(-time.Hour))

	f.step(t)

	if node.Phase != upgradev1alpha1.ConfigNodeReverted {
		t.Fatalf("phase = %q, want Reverted", node.Phase)
	}

	callsBefore := len(f.node.Calls)

	node.TryDeadline = ptrTime(time.Now().Add(-time.Second))
	f.node.Unreachable = false
	f.node.Reboot()

	f.step(t)

	if node.Phase != upgradev1alpha1.ConfigNodeFailed {
		t.Fatalf("phase = %q, want Failed after a confirmed revert", node.Phase)
	}

	if !bytes.Equal(f.node.SlotBytes(confignode.SlotPersistent), canonical(t, baseConfig("old-name"))) {
		t.Error("the node must be back on its last known good config")
	}

	for _, call := range f.node.Calls[callsBefore:] {
		if strings.HasPrefix(call, "Apply") {
			t.Errorf("controller issued %q on the recovery path; recovery must need no writes", call)
		}
	}
}

func TestRejectedConfigFailsImmediately(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	f.node.RejectApply = true

	if _, err := f.r.startConfigNode(context.Background(), f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()
	if node.Phase != upgradev1alpha1.ConfigNodeFailed {
		t.Fatalf("phase = %q, want Failed", node.Phase)
	}

	if !bytes.Equal(f.node.SlotBytes(confignode.SlotPersistent), canonical(t, baseConfig("old-name"))) {
		t.Error("a rejected config must leave the node untouched")
	}

	if f.plan.Status.Phase != upgradev1alpha1.ConfigPlanFrozen {
		t.Errorf("plan phase = %q, want Frozen", f.plan.Status.Phase)
	}
}

func TestRejectedCommitSaysTheNodeKeepsTheTry(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")

	if _, err := f.r.startConfigNode(context.Background(), f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()
	for node.Phase != upgradev1alpha1.ConfigNodeCommitting {
		if f.step(t); node.Phase == upgradev1alpha1.ConfigNodeReverted || node.Phase == upgradev1alpha1.ConfigNodeFailed {
			t.Fatalf("setup: phase = %s (%s)", node.Phase, node.Message)
		}
	}

	f.node.RejectApply = true
	if f.step(t); node.Phase != upgradev1alpha1.ConfigNodeFailed {
		t.Fatalf("phase = %s, want Failed", node.Phase)
	}

	if f.node.TryPending() || !strings.Contains(node.Message, "until its next reboot") {
		t.Errorf("try pending = %v, message %q: want no timer and a message saying the tried config stays until a reboot",
			f.node.TryPending(), node.Message)
	}
}

func TestCommitAbortsWhenTryDeadlinePassed(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()
	setNode(node, upgradev1alpha1.ConfigNodeCommitting, "ready to commit")

	node.TryDeadline = ptrTime(time.Now().Add(-time.Second))

	f.step(t)

	if node.Phase != upgradev1alpha1.ConfigNodeReverted {
		t.Fatalf("phase = %q, want Reverted; committing past the deadline would tear the slots", node.Phase)
	}

	if !bytes.Equal(f.node.SlotBytes(confignode.SlotPersistent), canonical(t, baseConfig("old-name"))) {
		t.Error("nothing may be persisted once the deadline has passed")
	}
}

func TestCommitAbortsWhenNodeAlreadyReverted(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()
	setNode(node, upgradev1alpha1.ConfigNodeCommitting, "ready to commit")

	node.TryDeadline = ptrTime(time.Now().Add(time.Hour))
	f.node.Reboot()

	f.step(t)

	if node.Phase != upgradev1alpha1.ConfigNodeReverted {
		t.Fatalf("phase = %q, want Reverted; the active-slot re-read must catch this", node.Phase)
	}

	if !bytes.Equal(f.node.SlotBytes(confignode.SlotPersistent), canonical(t, baseConfig("old-name"))) {
		t.Error("nothing may be persisted when the node is not running the tried config")
	}
}

func TestCommitAbortsWhenSourceChangedMidFlight(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()
	setNode(node, upgradev1alpha1.ConfigNodeCommitting, "ready to commit")
	node.TryDeadline = ptrTime(time.Now().Add(time.Hour))

	f.r.SourceFor = func(*upgradev1alpha1.MachineConfigPlan) (configsource.Fetcher, error) {
		return staticFetcher{fsys: sourceFS("third-name")}, nil
	}

	f.step(t)

	if node.Phase != upgradev1alpha1.ConfigNodeReverted {
		t.Fatalf("phase = %q, want Reverted", node.Phase)
	}

	if bytes.Equal(f.node.SlotBytes(confignode.SlotPersistent), canonical(t, baseConfig("third-name"))) {
		t.Error("committed a config that was never verified on the node")
	}
}

func TestCommitWaitsForTheRevertWhenTheTreeBreaks(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()
	setNode(node, upgradev1alpha1.ConfigNodeCommitting, "ready to commit")

	f.r.SourceFor = func(*upgradev1alpha1.MachineConfigPlan) (configsource.Fetcher, error) {
		return staticFetcher{fsys: fstest.MapFS{"base/00-v1alpha1.yaml": &fstest.MapFile{Data: []byte("machine: [")}}}, nil
	}

	f.step(t)

	if node.Phase != upgradev1alpha1.ConfigNodeReverted {
		t.Fatalf("phase = %q (%s), want Reverted while the try timer runs", node.Phase, node.Message)
	}

	if !f.node.TryPending() {
		t.Fatal("the test needs the try timer still armed")
	}
}

func TestUnhealthyClusterRevertsInsteadOfCommitting(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	f.plan.Spec.Verify = &upgradev1alpha1.ConfigVerifyTimeouts{
		SoakSeconds:          secs(0),
		TryTimeoutSeconds:    secs(600),
		ProbeTimeoutSeconds:  secs(60),
		HealthTimeoutSeconds: secs(60),
	}

	f.r.HealthCheck = func(context.Context) (bool, string, error) {
		return false, "etcd has lost quorum", nil
	}

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()

	f.step(t)

	if node.Phase != upgradev1alpha1.ConfigNodeHealthChecking {
		t.Fatalf("phase = %q, want HealthChecking", node.Phase)
	}

	node.LastTransition = metav1.NewTime(time.Now().Add(-time.Hour))
	node.TryIssuedAt = ptrTime(time.Now().Add(-time.Hour))

	f.step(t)

	if node.Phase != upgradev1alpha1.ConfigNodeReverted {
		t.Fatalf("phase = %q, want Reverted", node.Phase)
	}

	if !bytes.Equal(f.node.SlotBytes(confignode.SlotPersistent), canonical(t, baseConfig("old-name"))) {
		t.Error("an unhealthy cluster must never result in a persisted config")
	}
}

func TestUnconfirmedRevertFailsLoudly(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()
	setNode(node, upgradev1alpha1.ConfigNodeReverted, "probe timed out")

	node.TryDeadline = ptrTime(time.Now().Add(-time.Hour))
	f.node.Unreachable = true

	f.step(t)

	if node.Phase != upgradev1alpha1.ConfigNodeFailed {
		t.Fatalf("phase = %q, want Failed", node.Phase)
	}

	if !strings.Contains(node.Message, "human eyes") {
		t.Errorf("an unconfirmed revert must say so plainly, got %q", node.Message)
	}
}

func TestRevertWaitsOutTheGracePeriod(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()
	setNode(node, upgradev1alpha1.ConfigNodeReverted, "probe timed out")

	node.TryDeadline = ptrTime(time.Now().Add(-time.Second))
	f.node.Unreachable = true

	f.step(t)

	if node.Phase != upgradev1alpha1.ConfigNodeReverted {
		t.Errorf("phase = %q, want Reverted; the revert is not late yet", node.Phase)
	}
}

func TestProbeRejectsWrongActiveConfig(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()

	f.node.Reboot()

	f.step(t)

	if node.Phase == upgradev1alpha1.ConfigNodeHealthChecking {
		t.Fatal("a reachable node running the wrong config must not pass the probe")
	}
}

func TestLandedCommitIsNotAFailedRevert(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()
	f.step(t)
	f.step(t)

	persisted := *node.DeepCopy()

	if f.step(t); node.Phase != upgradev1alpha1.ConfigNodeSucceeded {
		t.Fatalf("after commit, phase = %q, want Succeeded", node.Phase)
	}

	*node = persisted
	node.TryDeadline = ptrTime(time.Now().Add(-time.Hour))

	f.step(t)

	if node.Phase != upgradev1alpha1.ConfigNodeSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded: the commit landed", node.Phase, node.Message)
	}
}

func TestControllerOutageDoesNotRevertAGoodTry(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()
	node.LastTransition = metav1.NewTime(time.Now().Add(-probeTimeout(f.plan) - time.Minute))

	if f.step(t); node.Phase != upgradev1alpha1.ConfigNodeHealthChecking {
		t.Fatalf("after an outage longer than the probe budget, phase = %q (%s), want HealthChecking", node.Phase, node.Message)
	}

	away := metav1.NewTime(time.Now().Add(-healthTimeout(f.plan) - time.Minute))
	node.LastTransition, node.TryIssuedAt = away, &away

	if f.step(t); node.Phase != upgradev1alpha1.ConfigNodeCommitting {
		t.Fatalf("after an outage longer than the health budget, phase = %q (%s), want Committing", node.Phase, node.Message)
	}
}

func TestTryingPhaseResumesByProbingNotReapplying(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()
	setNode(node, upgradev1alpha1.ConfigNodeTrying, "apply issued")

	applies := 0

	for _, c := range f.node.Calls {
		if c == callApplyTry {
			applies++
		}
	}

	f.step(t)

	after := 0

	for _, c := range f.node.Calls {
		if c == callApplyTry {
			after++
		}
	}

	if after != applies {
		t.Errorf("resuming from Trying issued another apply (%d -> %d)", applies, after)
	}
}

func TestUnknownPhaseIsAnError(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	node := f.nodeStatus()
	node.Phase = upgradev1alpha1.ConfigNodePhase("Nonsense")

	if _, err := f.r.driveConfigNode(context.Background(), f.plan, node, f.source(t)); err == nil {
		t.Error("an unrecognised phase must surface as an error, not be silently ignored")
	}
}

func TestDiffOnlyModeNeverApplies(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	f.plan.Spec.Mode = upgradev1alpha1.ModeDiffOnly

	if _, err := f.r.reconcilePlan(context.Background(), f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, call := range f.node.Calls {
		if strings.HasPrefix(call, "Apply") {
			t.Fatalf("DiffOnly mode issued %q", call)
		}
	}

	if f.plan.Status.Phase != upgradev1alpha1.ConfigPlanDiffing {
		t.Errorf("plan phase = %q, want Diffing", f.plan.Status.Phase)
	}

	if !bytes.Equal(f.node.SlotBytes(confignode.SlotPersistent), canonical(t, baseConfig("old-name"))) {
		t.Error("DiffOnly must leave the node untouched")
	}
}

func TestPausedPlanStartsNothing(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	f.plan.Spec.Paused = true

	if _, err := f.r.reconcilePlan(context.Background(), f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, call := range f.node.Calls {
		if strings.HasPrefix(call, "Apply") {
			t.Fatalf("paused plan issued %q", call)
		}
	}

	if f.plan.Status.Reason != reasonConfigPaused {
		t.Errorf("reason = %q, want %q", f.plan.Status.Reason, reasonConfigPaused)
	}
}

func TestImpossibleTryBudgetFreezesBeforeApplying(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	f.plan.Spec.Verify = &upgradev1alpha1.ConfigVerifyTimeouts{
		SoakSeconds:          secs(0),
		TryTimeoutSeconds:    secs(60),
		ProbeTimeoutSeconds:  secs(60),
		HealthTimeoutSeconds: secs(300),
	}

	if _, err := f.r.reconcilePlan(context.Background(), f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, call := range f.node.Calls {
		if strings.HasPrefix(call, "Apply") {
			t.Fatalf("applied despite an unmeetable try budget: %q", call)
		}
	}

	if f.plan.Status.Phase != upgradev1alpha1.ConfigPlanFrozen {
		t.Errorf("plan phase = %q, want Frozen", f.plan.Status.Phase)
	}
}

func TestMatchingNodeNeedsNoWork(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "same-name", "same-name")

	if _, err := f.r.reconcilePlan(context.Background(), f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, call := range f.node.Calls {
		if strings.HasPrefix(call, "Apply") {
			t.Fatalf("applied to a node that already matched: %q", call)
		}
	}

	if f.plan.Status.Phase != upgradev1alpha1.ConfigPlanIdle {
		t.Errorf("plan phase = %q, want Idle", f.plan.Status.Phase)
	}
}

func TestSourceErrorTouchesNothing(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	f.r.SourceFor = func(*upgradev1alpha1.MachineConfigPlan) (configsource.Fetcher, error) {
		return staticFetcher{err: fmt.Errorf("git unreachable")}, nil
	}

	if _, err := f.r.reconcilePlan(context.Background(), f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, call := range f.node.Calls {
		if strings.HasPrefix(call, "Apply") {
			t.Fatalf("applied despite an unreadable source: %q", call)
		}
	}

	if f.plan.Status.Reason != reasonConfigSourceError {
		t.Errorf("reason = %q, want %q", f.plan.Status.Reason, reasonConfigSourceError)
	}

	if f.plan.Status.Phase != upgradev1alpha1.ConfigPlanWaiting {
		t.Errorf("an unreachable source is a gate, not a failure; phase = %q", f.plan.Status.Phase)
	}
}

func TestFailureFreezesTheWholePlan(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")

	f.plan.Status.Nodes = append(f.plan.Status.Nodes, upgradev1alpha1.ConfigNodeStatus{
		Name:    "worker-1",
		Address: "10.0.0.11",
		Role:    upgradev1alpha1.RoleWorker,
		Phase:   upgradev1alpha1.ConfigNodeFailed,
		Message: "rejected earlier",
	})

	if _, err := f.r.reconcilePlan(context.Background(), f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, call := range f.node.Calls {
		if strings.HasPrefix(call, "Apply") {
			t.Fatalf("a frozen plan applied to another node: %q", call)
		}
	}

	if f.plan.Status.Phase != upgradev1alpha1.ConfigPlanFrozen {
		t.Errorf("plan phase = %q, want Frozen", f.plan.Status.Phase)
	}
}

func TestUnfreezeAnnotationResetsAndIsConsumed(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	node := f.nodeStatus()
	setNode(node, upgradev1alpha1.ConfigNodeFailed, "rejected")
	node.TryDeadline = ptrTime(time.Now())
	node.TriedHash = "sha256:stale"

	f.plan.Annotations = map[string]string{
		upgradev1alpha1.UnfreezeAnnotation: testWorkerName,
	}

	handled, err := f.r.handleUnfreeze(ctx, f.plan)
	if err != nil {
		t.Fatalf("unfreeze: %v", err)
	}

	if !handled {
		t.Fatal("expected the annotation to be handled")
	}

	if node.Phase != upgradev1alpha1.ConfigNodePending {
		t.Errorf("phase = %q, want Pending", node.Phase)
	}

	if node.TryDeadline != nil || node.TriedHash != "" {
		t.Error("stale try bookkeeping must be cleared on unfreeze")
	}

	if _, still := f.plan.Annotations[upgradev1alpha1.UnfreezeAnnotation]; still {
		t.Error("the annotation must be consumed so the acknowledgement is single-use")
	}
}

func TestObserveKeepsInFlightNodeThatVanished(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	node := f.nodeStatus()
	setNode(node, upgradev1alpha1.ConfigNodeProbing, "in flight")
	node.TryDeadline = ptrTime(time.Now().Add(time.Hour))

	if err := f.r.observeNodes(ctx, f.plan); err != nil {
		t.Fatalf("observe: %v", err)
	}

	if len(f.plan.Status.Nodes) != 1 {
		t.Fatalf("in-flight node was dropped; got %d nodes", len(f.plan.Status.Nodes))
	}

	if f.plan.Status.Nodes[0].TryDeadline == nil {
		t.Error("the try deadline must survive, or nobody knows when the node reverts")
	}
}

func TestObserveDropsVanishedIdleNode(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")

	f.plan.Status.Nodes = append(f.plan.Status.Nodes, upgradev1alpha1.ConfigNodeStatus{
		Name:    "worker-gone",
		Address: "10.0.0.99",
		Role:    upgradev1alpha1.RoleWorker,
		Phase:   upgradev1alpha1.ConfigNodeSucceeded,
	})

	if err := f.r.observeNodes(context.Background(), f.plan); err != nil {
		t.Fatalf("observe: %v", err)
	}

	for _, n := range f.plan.Status.Nodes {
		if n.Name == "worker-gone" {
			t.Error("a vanished Succeeded node should be dropped")
		}
	}
}

func TestObserveKeepsVanishedFailedNode(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")

	f.plan.Status.Nodes = append(f.plan.Status.Nodes, upgradev1alpha1.ConfigNodeStatus{
		Name:    "worker-gone",
		Address: "10.0.0.99",
		Role:    upgradev1alpha1.RoleWorker,
		Phase:   upgradev1alpha1.ConfigNodeFailed,
		Message: "rejected",
	})

	if err := f.r.observeNodes(context.Background(), f.plan); err != nil {
		t.Fatalf("observe: %v", err)
	}

	if failedNode(f.plan) == nil {
		t.Error("dropping a vanished Failed node silently unfroze the plan")
	}
}

func TestObserveAssignsRolesAndAddresses(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := upgradev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}

	cp := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "control-plane-0",
			Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
		},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
		}},
	}
	worker := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: testWorkerName},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: testWorkerAddr},
		}},
	}
	addressless := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "ghost"}}

	r := &MachineConfigPlanReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(cp, worker, addressless).Build(),
	}

	plan := &upgradev1alpha1.MachineConfigPlan{}
	if err := r.observeNodes(context.Background(), plan); err != nil {
		t.Fatalf("observe: %v", err)
	}

	if len(plan.Status.Nodes) != 2 {
		t.Fatalf("got %d nodes, want 2 (the addressless one is skipped)", len(plan.Status.Nodes))
	}

	byName := map[string]upgradev1alpha1.ConfigNodeStatus{}
	for _, n := range plan.Status.Nodes {
		byName[n.Name] = n
	}

	if byName["control-plane-0"].Role != upgradev1alpha1.RoleControlPlane {
		t.Error("control-plane label was not recognised")
	}

	if byName[testWorkerName].Role != upgradev1alpha1.RoleWorker {
		t.Error("unlabelled node should be a worker")
	}

	if byName[testWorkerName].Address != testWorkerAddr {
		t.Errorf("address = %q, want %q", byName[testWorkerName].Address, testWorkerAddr)
	}
}

func TestPendingNodeCarriesADiff(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	f.plan.Spec.Mode = upgradev1alpha1.ModeDiffOnly

	if _, err := f.r.reconcilePlan(context.Background(), f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	node := f.nodeStatus()
	if node.Phase != upgradev1alpha1.ConfigNodePending {
		t.Fatalf("phase = %q, want Pending", node.Phase)
	}

	if node.Diff == "" {
		t.Fatal("a pending node must carry a diff")
	}

	if !strings.Contains(node.Diff, "new-name") {
		t.Errorf("diff should show the incoming hostname, got:\n%s", node.Diff)
	}

	if node.DesiredHash == "" || node.ObservedHash == "" {
		t.Error("both hashes should be recorded")
	}

	if node.DesiredHash == node.ObservedHash {
		t.Error("a pending node must have differing hashes")
	}
}

func TestReconcileIgnoresNonSingleton(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")

	res, err := f.r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: "not-cluster"},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if res.RequeueAfter != 0 {
		t.Error("a non-singleton request should be ignored outright")
	}

	if len(f.node.Calls) != 0 {
		t.Errorf("a non-singleton request touched a node: %v", f.node.Calls)
	}
}

func TestFailureMessageKeepsTheRevertReason(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	f.plan.Spec.Verify = &upgradev1alpha1.ConfigVerifyTimeouts{
		SoakSeconds:          secs(0),
		TryTimeoutSeconds:    secs(600),
		ProbeTimeoutSeconds:  secs(60),
		HealthTimeoutSeconds: secs(60),
	}

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()

	f.node.Unreachable = true
	node.LastTransition = metav1.NewTime(time.Now().Add(-time.Hour))

	f.step(t)

	if node.Phase != upgradev1alpha1.ConfigNodeReverted {
		t.Fatalf("phase = %q, want Reverted", node.Phase)
	}

	if !strings.Contains(node.RevertReason, "did not confirm") {
		t.Errorf("revert reason should say what went wrong, got %q", node.RevertReason)
	}

	node.TryDeadline = ptrTime(time.Now().Add(-time.Second))
	f.node.Unreachable = false
	f.node.Reboot()

	f.step(t)

	if node.Phase != upgradev1alpha1.ConfigNodeFailed {
		t.Fatalf("phase = %q, want Failed", node.Phase)
	}

	if !strings.Contains(node.Message, "did not confirm") {
		t.Errorf("failure message lost the cause, got %q", node.Message)
	}

	if strings.Contains(node.Message, "waiting for the node to revert") {
		t.Errorf("failure message reports the polling status as the cause: %q", node.Message)
	}
}

func partialFS(hostname string) fstest.MapFS {
	return fstest.MapFS{
		"base/00-hostname.yaml": &fstest.MapFile{
			Data: []byte("machine:\n  network:\n    hostname: " + hostname + "\n"),
		},
	}
}

func (f *driveFixture) useSource(fsys fstest.MapFS) {
	f.r.SourceFor = func(*upgradev1alpha1.MachineConfigPlan) (configsource.Fetcher, error) {
		return staticFetcher{fsys: fsys}, nil
	}
}

func TestMergePartialTreeRollsOut(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "unused")
	f.useSource(partialFS("new-name"))

	ctx := context.Background()

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()
	for range 10 {
		if !node.Phase.InFlight() {
			break
		}

		f.step(t)
	}

	if node.Phase != upgradev1alpha1.ConfigNodeSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", node.Phase, node.Message)
	}

	if !bytes.Equal(f.node.SlotBytes(confignode.SlotPersistent), canonical(t, baseConfig("new-name"))) {
		t.Error("merged commit should equal the node's config with only the hostname changed")
	}
}

func TestMergePartialTreeInSyncIsIdle(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "same-name", "unused")
	f.useSource(partialFS("same-name"))

	if _, err := f.r.reconcilePlan(context.Background(), f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if f.plan.Status.Phase != upgradev1alpha1.ConfigPlanIdle {
		t.Fatalf("plan phase = %q (%s), want Idle", f.plan.Status.Phase, f.plan.Status.Message)
	}

	for _, call := range f.node.Calls {
		if strings.HasPrefix(call, "Apply") {
			t.Fatalf("applied to a node that already matched: %q", call)
		}
	}
}

func TestMergeDiffShowsOnlyManagedFields(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "unused")
	f.useSource(partialFS("new-name"))
	f.plan.Spec.Mode = upgradev1alpha1.ModeDiffOnly

	if _, err := f.r.reconcilePlan(context.Background(), f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	diff := f.nodeStatus().Diff
	if !strings.Contains(diff, "new-name") {
		t.Fatalf("diff should show the hostname change, got:\n%s", diff)
	}

	for _, line := range strings.Split(diff, "\n") {
		if (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-")) &&
			!strings.HasPrefix(line, "+++") && !strings.HasPrefix(line, "---") &&
			!strings.Contains(line, "hostname") {
			t.Errorf("diff touches a field the tree does not manage: %q", line)
		}
	}
}

func TestMergeCommitAbortsWhenPersistentMoved(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "unused")
	f.useSource(partialFS("new-name"))

	ctx := context.Background()

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()
	setNode(node, upgradev1alpha1.ConfigNodeCommitting, "ready to commit")
	node.TryDeadline = ptrTime(time.Now().Add(time.Hour))

	if _, err := f.node.ApplyCommit(ctx, canonical(t, baseConfig("someone-else"))); err != nil {
		t.Fatalf("simulate external write: %v", err)
	}

	f.step(t)

	if node.Phase != upgradev1alpha1.ConfigNodeReverted {
		t.Fatalf("phase = %q, want Reverted", node.Phase)
	}

	if bytes.Equal(f.node.SlotBytes(confignode.SlotPersistent), canonical(t, baseConfig("new-name"))) {
		t.Error("committed a render whose base changed during verification")
	}
}

func TestSoakHoldsTheCommit(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	f.plan.Spec.Verify = &upgradev1alpha1.ConfigVerifyTimeouts{SoakSeconds: secs(120)}
	ctx := context.Background()

	healthCalls := 0
	f.r.HealthCheck = func(context.Context) (bool, string, error) {
		healthCalls++

		return true, "", nil
	}

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()

	f.step(t)

	if node.Phase != upgradev1alpha1.ConfigNodeHealthChecking {
		t.Fatalf("phase = %q, want HealthChecking", node.Phase)
	}

	res := f.step(t)

	if node.Phase != upgradev1alpha1.ConfigNodeHealthChecking {
		t.Fatalf("committed inside the soak: phase = %q", node.Phase)
	}

	if regexp.MustCompile(`\d+s\b`).MatchString(node.Message) {
		t.Errorf("soak message holds a countdown: %q", node.Message)
	}

	if healthCalls != 0 {
		t.Error("the deciding health check must not run before the soak ends")
	}

	if res.RequeueAfter <= 0 || res.RequeueAfter > 2*time.Minute {
		t.Errorf("should requeue for the rest of the soak, got %s", res.RequeueAfter)
	}

	node.TryIssuedAt = ptrTime(time.Now().Add(-3 * time.Minute))

	f.step(t)

	if node.Phase != upgradev1alpha1.ConfigNodeCommitting {
		t.Fatalf("phase = %q, want Committing once the soak is over", node.Phase)
	}

	if healthCalls != 1 {
		t.Errorf("health ran %d times, want once after the soak", healthCalls)
	}
}

func TestSoakDoesNotConsumeTheHealthBudget(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	f.plan.Spec.Verify = &upgradev1alpha1.ConfigVerifyTimeouts{
		SoakSeconds:          secs(300),
		HealthTimeoutSeconds: secs(60),
	}
	f.r.HealthCheck = func(context.Context) (bool, string, error) { return false, "settling", nil }

	if _, err := f.r.startConfigNode(context.Background(), f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()
	f.step(t)

	node.LastTransition = metav1.NewTime(time.Now().Add(-4 * time.Minute))
	node.TryIssuedAt = ptrTime(time.Now().Add(-4 * time.Minute))

	f.step(t)

	if node.Phase != upgradev1alpha1.ConfigNodeHealthChecking {
		t.Errorf("phase = %q; time spent soaking must not count against the health timeout", node.Phase)
	}
}

func TestRenderWarningsReachStatus(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "same-name", "unused")
	f.useSource(fstest.MapFS{
		"base/00-hostname.yaml": &fstest.MapFile{Data: []byte("machine:\n  network:\n    hostname: same-name\n")},
		"nodes/worker-0.yaml":   &fstest.MapFile{Data: []byte("machine:\n  sysctls:\n    not.there:\n      $patch: delete\n")},
	})

	if _, err := f.r.reconcilePlan(context.Background(), f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if f.plan.Status.Phase != upgradev1alpha1.ConfigPlanIdle {
		t.Fatalf("phase = %q (%s), want Idle", f.plan.Status.Phase, f.plan.Status.Message)
	}

	warnings := f.nodeStatus().Warnings
	if len(warnings) == 0 || !strings.Contains(strings.Join(warnings, " "), "removal already applied") {
		t.Errorf("expected the removal warning in status, got %v", warnings)
	}
}

func TestGitSourceReadsSecret(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "unused")

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "talos-config-git", Namespace: "talos-lifecycle-system"},
		Data: map[string][]byte{
			"identity":    []byte("PRIVATE"),
			"known_hosts": []byte("gitlab.example.com ssh-ed25519 AAAA"),
		},
	}
	if err := f.r.Create(context.Background(), secret); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	plan := f.plan.DeepCopy()
	plan.Spec.Source = upgradev1alpha1.ConfigSource{Git: &upgradev1alpha1.GitSource{
		URL:  "ssh://git@gitlab.example.com/infra/talos-config.git",
		Ref:  "main",
		Path: "machineconfig",
		SecretRef: upgradev1alpha1.SecretReference{
			Name:      "talos-config-git",
			Namespace: "talos-lifecycle-system",
		},
	}}

	fetcher, err := f.r.gitFetcher(context.Background(), plan.Spec.Source.Git)
	if err != nil {
		t.Fatalf("defaultSourceFor: %v", err)
	}

	g, ok := fetcher.(configsource.GitFetcher)
	if !ok {
		t.Fatalf("got %T, want GitFetcher", fetcher)
	}

	if g.URL != plan.Spec.Source.Git.URL || g.Ref != "main" || g.Path != "machineconfig" {
		t.Errorf("fetcher fields not taken from spec: %+v", g)
	}

	if string(g.Identity) != "PRIVATE" || !strings.Contains(string(g.KnownHosts), "gitlab.example.com") {
		t.Error("credentials not taken from the Secret")
	}

	plan.Spec.Source.Git.SecretRef.Name = "nope"
	if _, err := f.r.gitFetcher(context.Background(), plan.Spec.Source.Git); err == nil {
		t.Error("expected an error for a missing secret")
	}
}

func TestTryIntentPersistedBeforeApply(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	var live upgradev1alpha1.MachineConfigPlan
	if err := f.r.Get(ctx, client.ObjectKey{Name: upgradev1alpha1.MachineConfigPlanName}, &live); err != nil {
		t.Fatalf("get: %v", err)
	}

	stored := live.Status.Nodes[0]
	if stored.Phase != upgradev1alpha1.ConfigNodeTrying || stored.TriedHash == "" || stored.TryDeadline == nil {
		t.Fatalf("the API server must hold Trying with hash and deadline before the TRY, got %+v", stored)
	}
}

const callApplyTry = "ApplyTry"

type failingTry struct{ *confignode.FakeNode }

func (failingTry) ApplyTry(context.Context, []byte, time.Duration) (string, error) {
	return "", errors.New("connection reset")
}

type dialerFunc func(context.Context, string) (confignode.Client, error)

func (d dialerFunc) Dial(ctx context.Context, a string) (confignode.Client, error) { return d(ctx, a) }

func TestTryTransportErrorIsNotRetried(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	f.r.Dialer = dialerFunc(func(context.Context, string) (confignode.Client, error) {
		return failingTry{f.node}, nil
	})

	if _, err := f.r.startConfigNode(context.Background(), f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	node := f.nodeStatus()
	if node.Phase != upgradev1alpha1.ConfigNodeReverted {
		t.Fatalf("phase = %q, want Reverted", node.Phase)
	}

	if node.TryDeadline == nil {
		t.Error("the deadline must be kept so the revert is confirmed after it")
	}
}

func TestMissingTryTimestamps(t *testing.T) {
	t.Parallel()

	t.Run("health checking without TryIssuedAt", func(t *testing.T) {
		t.Parallel()

		f := newDriveFixture(t, "old-name", "new-name")
		node := f.nodeStatus()
		setNode(node, upgradev1alpha1.ConfigNodeHealthChecking, "soaking")
		node.TryDeadline = ptrTime(time.Now().Add(time.Hour))

		f.step(t)

		if node.Phase != upgradev1alpha1.ConfigNodeCommitting {
			t.Errorf("phase = %q, want Committing", node.Phase)
		}
	})

	t.Run("reverted without TryDeadline", func(t *testing.T) {
		t.Parallel()

		f := newDriveFixture(t, "old-name", "new-name")
		node := f.nodeStatus()
		setNode(node, upgradev1alpha1.ConfigNodeReverted, "probe timed out")
		node.LastTransition = metav1.NewTime(time.Now().Add(-time.Hour))
		f.node.Unreachable = true

		f.step(t)

		if node.Phase != upgradev1alpha1.ConfigNodeFailed {
			t.Errorf("phase = %q; a revert with no deadline must still be bounded", node.Phase)
		}
	})
}

func TestTryIntentWriteFailureSendsNothing(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	f.r.Client = interceptor.NewClient(f.r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			return errors.New("apiserver unavailable")
		},
	})

	if _, err := f.r.startConfigNode(context.Background(), f.plan, f.nodeStatus(), f.source(t)); err == nil {
		t.Fatal("expected the failed intent write to surface")
	}

	for _, call := range f.node.Calls {
		if call == callApplyTry {
			t.Fatal("TRY was sent although the intent could not be recorded")
		}
	}

	node := f.nodeStatus()
	if node.Phase != upgradev1alpha1.ConfigNodePending || node.TriedHash != "" || node.TryDeadline != nil {
		t.Errorf("node must be back to a clean Pending, got %+v", node)
	}
}

func TestRebootOnlyChangeIsFlaggedNotApplied(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "same-name", "same-name")
	fsys := sourceFS("same-name")
	fsys["base/10-env.yaml"] = &fstest.MapFile{Data: []byte("machine:\n  env:\n    HTTP_PROXY: http://proxy:3128\n")}
	f.r.SourceFor = func(*upgradev1alpha1.MachineConfigPlan) (configsource.Fetcher, error) {
		return staticFetcher{fsys: fsys}, nil
	}

	if _, err := f.r.reconcilePlan(context.Background(), f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, call := range f.node.Calls {
		if strings.HasPrefix(call, "Apply") {
			t.Fatalf("issued %q for a reboot-only change", call)
		}
	}

	node := f.nodeStatus()
	if node.Phase != upgradev1alpha1.ConfigNodeSucceeded {
		t.Errorf("phase = %q, want Succeeded", node.Phase)
	}

	if len(node.Skipped) != 1 || node.Skipped[0] != "v1alpha1: machine.env" {
		t.Errorf("skipped = %v, want [v1alpha1: machine.env]", node.Skipped)
	}

	if f.plan.Status.Reason != reasonConfigRebootOnlySkipped {
		t.Errorf("reason = %q, want %q", f.plan.Status.Reason, reasonConfigRebootOnlySkipped)
	}
}

func TestRebootedNodeIsStillManaged(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	if _, err := f.r.startConfigNode(ctx, f.plan, f.nodeStatus(), f.source(t)); err != nil {
		t.Fatalf("start: %v", err)
	}

	for range 3 {
		f.step(t)
	}

	if f.nodeStatus().Phase != upgradev1alpha1.ConfigNodeSucceeded {
		t.Fatalf("setup: phase = %q, want Succeeded", f.nodeStatus().Phase)
	}

	f.node.Reboot()

	if _, err := f.r.reconcilePlan(ctx, f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if n := f.nodeStatus(); n.Phase != upgradev1alpha1.ConfigNodeSucceeded || strings.Contains(n.Message, "could not read") {
		t.Fatalf("after reboot: phase %q, message %q; want Succeeded and readable", n.Phase, n.Message)
	}

	f.nodeStatus().CompletedAt = nil

	fsys := sourceFS("newer-name")
	f.r.SourceFor = func(*upgradev1alpha1.MachineConfigPlan) (configsource.Fetcher, error) {
		return staticFetcher{fsys: fsys}, nil
	}

	if _, err := f.r.reconcilePlan(ctx, f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if f.nodeStatus().Phase != upgradev1alpha1.ConfigNodeProbing {
		t.Errorf("source moved after a reboot: phase = %q (%s), want Probing", f.nodeStatus().Phase, f.nodeStatus().Message)
	}
}

func TestReadErrorClearsOnceReadable(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "same-name", "same-name")
	ctx := context.Background()
	f.nodeStatus().Phase = upgradev1alpha1.ConfigNodeSucceeded

	f.node.Unreachable = true
	if _, err := f.r.reconcilePlan(ctx, f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if !strings.HasPrefix(f.nodeStatus().Message, msgReadFailed) {
		t.Fatalf("message = %q, want the read error", f.nodeStatus().Message)
	}

	f.node.Unreachable = false
	if _, err := f.r.reconcilePlan(ctx, f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if strings.HasPrefix(f.nodeStatus().Message, msgReadFailed) {
		t.Errorf("message = %q, the read error should be gone", f.nodeStatus().Message)
	}
}

func TestPendingMessageFollowsTheTree(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()
	f.plan.Spec.Paused = true

	tree := sourceFS("new-name")
	f.r.SourceFor = func(*upgradev1alpha1.MachineConfigPlan) (configsource.Fetcher, error) {
		return staticFetcher{fsys: tree}, nil
	}

	if _, err := f.r.reconcilePlan(ctx, f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	first := f.plan.Status.SourceRevision
	tree = sourceFS("newer-name")

	if _, err := f.r.reconcilePlan(ctx, f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if want := msgDiffers + f.plan.Status.SourceRevision; f.nodeStatus().Message != want || first == f.plan.Status.SourceRevision {
		t.Errorf("message = %q, want %q", f.nodeStatus().Message, want)
	}
}

func TestInFlightNodeFinishesWhenGitIsDown(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()

	if _, err := f.r.reconcilePlan(ctx, f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if f.nodeStatus().Phase != upgradev1alpha1.ConfigNodeProbing {
		t.Fatalf("setup: phase = %q, want Probing", f.nodeStatus().Phase)
	}

	f.r.SourceFor = func(*upgradev1alpha1.MachineConfigPlan) (configsource.Fetcher, error) {
		return staticFetcher{err: errors.New("gitlab down")}, nil
	}

	for range 3 {
		if _, err := f.r.reconcilePlan(ctx, f.plan); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	if f.nodeStatus().Phase != upgradev1alpha1.ConfigNodeSucceeded {
		t.Errorf("phase = %q (%s), want Succeeded on the last tree", f.nodeStatus().Phase, f.nodeStatus().Message)
	}

	if _, err := f.r.reconcilePlan(ctx, f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if f.plan.Status.Reason != reasonConfigSourceError {
		t.Errorf("reason = %q, want %q once nothing is in flight", f.plan.Status.Reason, reasonConfigSourceError)
	}
}

func TestTryUsesTheTreeAsItIsNow(t *testing.T) {
	t.Parallel()

	f := newDriveFixture(t, "old-name", "new-name")
	ctx := context.Background()
	tree := sourceFS("new-name")
	f.r.SourceFor = func(*upgradev1alpha1.MachineConfigPlan) (configsource.Fetcher, error) {
		return staticFetcher{fsys: tree}, nil
	}

	f.plan.Spec.Paused = true
	if _, err := f.r.reconcilePlan(ctx, f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	tree = sourceFS("fixed-name")
	f.plan.Spec.Paused = false

	if _, err := f.r.reconcilePlan(ctx, f.plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	want := confignode.HashBytes(canonical(t, baseConfig("fixed-name")))
	if got := f.nodeStatus().TriedHash; got != want {
		t.Errorf("tried %s, want the fixed tree %s", got, want)
	}
}
