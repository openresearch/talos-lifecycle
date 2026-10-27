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
	"strings"
	"testing"
	"time"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

func secs(v int64) *int64 { return &v }

func TestConfigTimeoutDefaults(t *testing.T) {
	t.Parallel()

	bare := &upgradev1alpha1.MachineConfigPlan{}

	if got := tryTimeout(bare); got != defaultTryTimeout {
		t.Errorf("tryTimeout default = %s, want %s", got, defaultTryTimeout)
	}

	if got := probeTimeout(bare); got != defaultProbeTimeout {
		t.Errorf("probeTimeout default = %s, want %s", got, defaultProbeTimeout)
	}

	if got := healthTimeout(bare); got != defaultHealthTimeout {
		t.Errorf("healthTimeout default = %s, want %s", got, defaultHealthTimeout)
	}

	tuned := &upgradev1alpha1.MachineConfigPlan{
		Spec: upgradev1alpha1.MachineConfigPlanSpec{
			Verify: &upgradev1alpha1.ConfigVerifyTimeouts{
				TryTimeoutSeconds:    secs(1200),
				ProbeTimeoutSeconds:  secs(90),
				HealthTimeoutSeconds: secs(600),
			},
		},
	}

	if got := tryTimeout(tuned); got != 20*time.Minute {
		t.Errorf("tryTimeout = %s, want 20m", got)
	}

	if got := probeTimeout(tuned); got != 90*time.Second {
		t.Errorf("probeTimeout = %s, want 90s", got)
	}

	partial := &upgradev1alpha1.MachineConfigPlan{
		Spec: upgradev1alpha1.MachineConfigPlanSpec{
			Verify: &upgradev1alpha1.ConfigVerifyTimeouts{TryTimeoutSeconds: secs(1200)},
		},
	}

	if got := probeTimeout(partial); got != defaultProbeTimeout {
		t.Errorf("unset field should fall back, got %s", got)
	}
}

func TestTryBudgetSafe(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                     string
		try, probe, health, soak int64 // 0 means unset, except soak where -1 is unset
		want                     bool
	}{
		{"defaults are safe", 0, 0, 0, -1, true},
		{"generous budget", 900, 60, 300, 0, true},
		{"exactly at the headroom boundary", 420, 60, 300, 0, true},
		{"one second short of headroom", 419, 60, 300, 0, false},
		{"try shorter than verification", 120, 60, 300, 0, false},
		{"soak counts against the budget", 420, 60, 300, 1, false},
		{"default soak with the old boundary", 420, 60, 300, -1, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			verify := &upgradev1alpha1.ConfigVerifyTimeouts{}
			if tc.try > 0 {
				verify.TryTimeoutSeconds = secs(tc.try)
			}

			if tc.probe > 0 {
				verify.ProbeTimeoutSeconds = secs(tc.probe)
			}

			if tc.health > 0 {
				verify.HealthTimeoutSeconds = secs(tc.health)
			}

			if tc.soak >= 0 {
				verify.SoakSeconds = secs(tc.soak)
			}

			plan := &upgradev1alpha1.MachineConfigPlan{
				Spec: upgradev1alpha1.MachineConfigPlanSpec{Verify: verify},
			}

			if got := tryBudgetSafe(plan); got != tc.want {
				t.Errorf("tryBudgetSafe = %v, want %v (try=%s verify=%s)",
					got, tc.want, tryTimeout(plan), verifyBudget(plan))
			}
		})
	}
}

func TestConfigModeDefaultsToDiffOnly(t *testing.T) {
	t.Parallel()

	if got := configMode(&upgradev1alpha1.MachineConfigPlan{}); got != upgradev1alpha1.ModeDiffOnly {
		t.Errorf("default mode = %q, want DiffOnly", got)
	}

	explicit := &upgradev1alpha1.MachineConfigPlan{
		Spec: upgradev1alpha1.MachineConfigPlanSpec{Mode: upgradev1alpha1.ModeReconcile},
	}

	if got := configMode(explicit); got != upgradev1alpha1.ModeReconcile {
		t.Errorf("explicit mode = %q, want Reconcile", got)
	}
}

func TestNextConfigNodeOrdersWorkersFirst(t *testing.T) {
	t.Parallel()

	plan := &upgradev1alpha1.MachineConfigPlan{
		Status: upgradev1alpha1.MachineConfigPlanStatus{
			Nodes: []upgradev1alpha1.ConfigNodeStatus{
				{Name: "control-plane-0", Role: upgradev1alpha1.RoleControlPlane, Phase: upgradev1alpha1.ConfigNodePending},
				{Name: "worker-2", Role: upgradev1alpha1.RoleWorker, Phase: upgradev1alpha1.ConfigNodePending},
				{Name: "worker-0", Role: upgradev1alpha1.RoleWorker, Phase: upgradev1alpha1.ConfigNodePending},
			},
		},
	}

	got := nextConfigNode(plan, "")

	switch {
	case got == nil:
		t.Fatal("expected a node to be selected")
	case got.Name != testWorkerName:
		t.Errorf("selected %q, want worker-0 (workers first, then by name)", got.Name)
	}

	plan.Status.Nodes[1].Phase = upgradev1alpha1.ConfigNodeSucceeded
	plan.Status.Nodes[2].Phase = upgradev1alpha1.ConfigNodeSucceeded

	got = nextConfigNode(plan, "")
	if got == nil || got.Name != "control-plane-0" {
		t.Errorf("expected control-plane-0 once workers are done, got %+v", got)
	}
}

func TestNextConfigNodeSkipsHeld(t *testing.T) {
	t.Parallel()

	plan := &upgradev1alpha1.MachineConfigPlan{
		Status: upgradev1alpha1.MachineConfigPlanStatus{
			Nodes: []upgradev1alpha1.ConfigNodeStatus{
				{Name: testWorkerName, Role: upgradev1alpha1.RoleWorker, Phase: upgradev1alpha1.ConfigNodePending, Held: true},
				{Name: "worker-3", Role: upgradev1alpha1.RoleWorker, Phase: upgradev1alpha1.ConfigNodePending},
			},
		},
	}

	if got := nextConfigNode(plan, ""); got == nil || got.Name != "worker-3" {
		t.Fatalf("got %+v, want worker-3 past the held worker-0", got)
	}

	plan.Status.Nodes[1].Phase = upgradev1alpha1.ConfigNodeSucceeded

	if got := nextConfigNode(plan, ""); got != nil {
		t.Fatalf("got %+v, want nil: only a held node is pending", got)
	}

	if got := heldConfigNodes(plan); len(got) != 1 || got[0] != testWorkerName {
		t.Errorf("heldConfigNodes = %v, want [worker-0]", got)
	}
}

func TestNextConfigNodeLeavesOwnNodeLast(t *testing.T) {
	t.Parallel()

	const otherWorker = "worker-1"

	plan := &upgradev1alpha1.MachineConfigPlan{
		Status: upgradev1alpha1.MachineConfigPlanStatus{
			Nodes: []upgradev1alpha1.ConfigNodeStatus{
				{Name: "control-plane-0", Role: upgradev1alpha1.RoleControlPlane, Phase: upgradev1alpha1.ConfigNodePending},
				{Name: testWorkerName, Role: upgradev1alpha1.RoleWorker, Phase: upgradev1alpha1.ConfigNodePending},
				{Name: otherWorker, Role: upgradev1alpha1.RoleWorker, Phase: upgradev1alpha1.ConfigNodePending},
			},
		},
	}

	if got := nextConfigNode(plan, testWorkerName); got == nil || got.Name != otherWorker {
		t.Fatalf("got %+v, want worker-1 ahead of the controller's own worker-0", got)
	}

	plan.Status.Nodes[2].Phase = upgradev1alpha1.ConfigNodeSucceeded

	if got := nextConfigNode(plan, testWorkerName); got == nil || got.Name != testWorkerName {
		t.Fatalf("got %+v, want worker-0: own node is last among workers, still before control planes", got)
	}
}

func TestNextConfigNodeIgnoresNonPending(t *testing.T) {
	t.Parallel()

	plan := &upgradev1alpha1.MachineConfigPlan{
		Status: upgradev1alpha1.MachineConfigPlanStatus{
			Nodes: []upgradev1alpha1.ConfigNodeStatus{
				{Name: "worker-0", Role: upgradev1alpha1.RoleWorker, Phase: upgradev1alpha1.ConfigNodeSucceeded},
				{Name: "worker-1", Role: upgradev1alpha1.RoleWorker, Phase: upgradev1alpha1.ConfigNodeFailed},
			},
		},
	}

	if got := nextConfigNode(plan, ""); got != nil {
		t.Errorf("expected no candidate, got %q", got.Name)
	}
}

func TestInFlightNodeAndPhaseHelpers(t *testing.T) {
	t.Parallel()

	inFlight := []upgradev1alpha1.ConfigNodePhase{
		upgradev1alpha1.ConfigNodeTrying,
		upgradev1alpha1.ConfigNodeProbing,
		upgradev1alpha1.ConfigNodeHealthChecking,
		upgradev1alpha1.ConfigNodeCommitting,
		upgradev1alpha1.ConfigNodeReverted,
	}

	for _, phase := range inFlight {
		if !phase.InFlight() {
			t.Errorf("phase %q must count as in flight, or the loop would start a second node under it", phase)
		}
	}

	for _, phase := range []upgradev1alpha1.ConfigNodePhase{
		upgradev1alpha1.ConfigNodePending,
		upgradev1alpha1.ConfigNodeSucceeded,
		upgradev1alpha1.ConfigNodeFailed,
	} {
		if phase.InFlight() {
			t.Errorf("phase %q must not count as in flight", phase)
		}
	}
}

func TestRevertedIsInFlight(t *testing.T) {
	t.Parallel()

	if !upgradev1alpha1.ConfigNodeReverted.InFlight() {
		t.Fatal("Reverted must be in flight, or the revert would never be confirmed")
	}

	plan := &upgradev1alpha1.MachineConfigPlan{
		Status: upgradev1alpha1.MachineConfigPlanStatus{
			Nodes: []upgradev1alpha1.ConfigNodeStatus{
				{Name: "worker-0", Phase: upgradev1alpha1.ConfigNodeReverted},
			},
		},
	}

	if got := findNode(plan, func(n *upgradev1alpha1.ConfigNodeStatus) bool { return n.Phase.InFlight() }); got == nil || got.Name != "worker-0" {
		t.Error("a reverting node must be reported as in flight")
	}
}

func TestConfigCooldownRemaining(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	cooldown := int64(10)

	plan := &upgradev1alpha1.MachineConfigPlan{
		Spec: upgradev1alpha1.MachineConfigPlanSpec{CooldownMinutes: &cooldown},
		Status: upgradev1alpha1.MachineConfigPlanStatus{
			Nodes: []upgradev1alpha1.ConfigNodeStatus{
				{Name: "worker-0", Phase: upgradev1alpha1.ConfigNodeSucceeded, CompletedAt: ptrTime(now.Add(-4 * time.Minute))},
			},
		},
	}

	if got := configCooldownRemaining(plan, now); got != 6*time.Minute {
		t.Errorf("remaining = %s, want 6m", got)
	}

	plan.Status.Nodes[0].CompletedAt = ptrTime(now.Add(-11 * time.Minute))
	if got := configCooldownRemaining(plan, now); got != 0 {
		t.Errorf("expired cooldown should be 0, got %s", got)
	}

	zero := int64(0)
	plan.Spec.CooldownMinutes = &zero
	plan.Status.Nodes[0].CompletedAt = ptrTime(now)

	if got := configCooldownRemaining(plan, now); got != 0 {
		t.Errorf("explicit zero cooldown should not wait, got %s", got)
	}

	plan.Spec.CooldownMinutes = &cooldown
	plan.Status.Nodes[0].CompletedAt = nil

	if got := configCooldownRemaining(plan, now); got != 0 {
		t.Errorf("no commit yet should not wait, got %s", got)
	}
}

func TestConfigCooldownSurvivesPhaseChange(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	cooldown := int64(10)

	plan := &upgradev1alpha1.MachineConfigPlan{
		Spec: upgradev1alpha1.MachineConfigPlanSpec{CooldownMinutes: &cooldown},
		Status: upgradev1alpha1.MachineConfigPlanStatus{
			Nodes: []upgradev1alpha1.ConfigNodeStatus{
				{Name: "a", Phase: upgradev1alpha1.ConfigNodeSucceeded, CompletedAt: ptrTime(now.Add(-time.Hour))},
				{Name: "b", Phase: upgradev1alpha1.ConfigNodePending, CompletedAt: ptrTime(now.Add(-2 * time.Minute))},
			},
		},
	}

	if got := configCooldownRemaining(plan, now); got != 8*time.Minute {
		t.Errorf("remaining = %s, want 8m (anchored on b's commit even though b is Pending again)", got)
	}
}

func TestTryDeadlinePassed(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)

	none := &upgradev1alpha1.ConfigNodeStatus{}
	if tryDeadlinePassed(none, now) {
		t.Error("a node with no deadline has not passed one")
	}

	future := &upgradev1alpha1.ConfigNodeStatus{TryDeadline: ptrTime(now.Add(time.Minute))}
	if tryDeadlinePassed(future, now) {
		t.Error("deadline in the future must not count as passed")
	}

	past := &upgradev1alpha1.ConfigNodeStatus{TryDeadline: ptrTime(now.Add(-time.Second))}
	if !tryDeadlinePassed(past, now) {
		t.Error("deadline in the past must count as passed; committing after it would tear the slots")
	}
}

func TestWindowGate(t *testing.T) {
	t.Parallel()

	at := func(hhmm string) time.Time {
		parsed, err := time.Parse("15:04", hhmm)
		if err != nil {
			t.Fatalf("bad test time %q: %v", hhmm, err)
		}

		return parsed
	}

	if gate, _ := windowGate(nil, at("12:00")); !gate.passed() {
		t.Error("no window means any time")
	}

	overnight := &upgradev1alpha1.UpgradeWindow{Start: "20:00", End: "05:00"}

	if gate, _ := windowGate(overnight, at("22:00")); !gate.passed() {
		t.Error("22:00 is inside a 20:00-05:00 window")
	}

	gate, wait := windowGate(overnight, at("12:00"))
	if gate.passed() || wait != 8*time.Hour || !strings.Contains(gate.reason, "opens at 20:00") {
		t.Errorf("12:00: passed=%v wait=%s reason=%q, want blocked for 8h, opening at 20:00", gate.passed(), wait, gate.reason)
	}

	if gate, _ := windowGate(&upgradev1alpha1.UpgradeWindow{Start: "25:00", End: "05:00"}, at("12:00")); gate.passed() {
		t.Error("an invalid window must block, not open")
	}
}

func TestFrozenConfigNode(t *testing.T) {
	t.Parallel()

	plan := &upgradev1alpha1.MachineConfigPlan{
		Status: upgradev1alpha1.MachineConfigPlanStatus{
			Nodes: []upgradev1alpha1.ConfigNodeStatus{
				{Name: "worker-0", Phase: upgradev1alpha1.ConfigNodeSucceeded},
				{Name: "worker-1", Phase: upgradev1alpha1.ConfigNodeFailed, Message: "rejected"},
			},
		},
	}

	got := failedNode(plan)
	if got == nil || got.Name != "worker-1" {
		t.Fatalf("expected worker-1 to freeze the plan, got %+v", got)
	}

	plan.Status.Nodes[1].Phase = upgradev1alpha1.ConfigNodeSucceeded
	if failedNode(plan) != nil {
		t.Error("no failed node means no freeze")
	}
}

func TestPendingNodes(t *testing.T) {
	t.Parallel()

	plan := &upgradev1alpha1.MachineConfigPlan{
		Status: upgradev1alpha1.MachineConfigPlanStatus{
			Nodes: []upgradev1alpha1.ConfigNodeStatus{
				{Name: "a", Phase: upgradev1alpha1.ConfigNodePending},
				{Name: "b", Phase: upgradev1alpha1.ConfigNodeSucceeded},
				{Name: "c", Phase: upgradev1alpha1.ConfigNodePending},
			},
		},
	}

	if got := countPending(plan); got != 2 {
		t.Errorf("got %d pending nodes, want 2", got)
	}
}
