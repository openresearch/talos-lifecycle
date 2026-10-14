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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

const (
	defaultTryTimeout     = 10 * time.Minute
	defaultProbeTimeout   = time.Minute
	defaultSoak           = 2 * time.Minute
	defaultHealthTimeout  = 5 * time.Minute
	defaultConfigCooldown = 5 * time.Minute

	commitHeadroom = time.Minute
	revertGrace    = 2 * time.Minute
	maxDiffBytes   = 8 * 1024

	configPollInterval = 10 * time.Second
	configIdleInterval = 2 * time.Minute
)

const (
	reasonConfigUpToDate          = "UpToDate"
	reasonConfigDiffOnly          = "DiffOnlyMode"
	reasonConfigPaused            = "Paused"
	reasonConfigRollingOut        = "RollingOut"
	reasonConfigWindow            = "WaitingForWindow"
	reasonConfigCooldown          = "CoolingDown"
	reasonConfigClusterUnready    = "WaitingForCluster"
	reasonConfigUpgradeInFlight   = "WaitingForUpgrade"
	reasonConfigRebootOnlySkipped = "RebootOnlySkipped"
	reasonConfigSourceError       = "SourceError"
	reasonConfigInvalid           = "InvalidConfig"
	reasonConfigNodeFailed        = "NodeFailed"
	reasonConfigUnfrozen          = "Unfrozen"
)

const msgReadFailed = "could not read the node's config: "

const msgDiffers = "config differs from revision "

const msgNotObserved = "not yet observed"

const unfreezeAll = "all"

func verify(plan *upgradev1alpha1.MachineConfigPlan) upgradev1alpha1.ConfigVerifyTimeouts {
	if plan.Spec.Verify == nil {
		return upgradev1alpha1.ConfigVerifyTimeouts{}
	}

	return *plan.Spec.Verify
}

func tryTimeout(p *upgradev1alpha1.MachineConfigPlan) time.Duration {
	return specDuration(verify(p).TryTimeoutSeconds, time.Second, defaultTryTimeout)
}

func probeTimeout(p *upgradev1alpha1.MachineConfigPlan) time.Duration {
	return specDuration(verify(p).ProbeTimeoutSeconds, time.Second, defaultProbeTimeout)
}

func soakTime(p *upgradev1alpha1.MachineConfigPlan) time.Duration {
	return specDuration(verify(p).SoakSeconds, time.Second, defaultSoak)
}

func healthTimeout(p *upgradev1alpha1.MachineConfigPlan) time.Duration {
	return specDuration(verify(p).HealthTimeoutSeconds, time.Second, defaultHealthTimeout)
}

func configCooldown(p *upgradev1alpha1.MachineConfigPlan) time.Duration {
	return specDuration(p.Spec.CooldownMinutes, time.Minute, defaultConfigCooldown)
}

func verifyBudget(p *upgradev1alpha1.MachineConfigPlan) time.Duration {
	return probeTimeout(p) + soakTime(p) + healthTimeout(p)
}

func tryBudgetSafe(p *upgradev1alpha1.MachineConfigPlan) bool {
	return tryTimeout(p) >= verifyBudget(p)+commitHeadroom
}

func configMode(p *upgradev1alpha1.MachineConfigPlan) string {
	if p.Spec.Mode == "" {
		return upgradev1alpha1.ModeDiffOnly
	}

	return p.Spec.Mode
}

// setNode: LastTransition is the clock for every timeout.
func setNode(n *upgradev1alpha1.ConfigNodeStatus, phase upgradev1alpha1.ConfigNodePhase, msg string) {
	n.Phase = phase
	n.Message = msg
	n.LastTransition = metav1.Now()
}

func setPlan(p *upgradev1alpha1.MachineConfigPlan, phase upgradev1alpha1.ConfigPlanPhase, reason, msg string) {
	p.Status.Phase = phase
	p.Status.Reason = reason
	p.Status.Message = msg
}

func findNode(
	p *upgradev1alpha1.MachineConfigPlan,
	match func(*upgradev1alpha1.ConfigNodeStatus) bool,
) *upgradev1alpha1.ConfigNodeStatus {
	for i := range p.Status.Nodes {
		if match(&p.Status.Nodes[i]) {
			return &p.Status.Nodes[i]
		}
	}

	return nil
}

func countPending(p *upgradev1alpha1.MachineConfigPlan) int {
	n := 0

	for i := range p.Status.Nodes {
		if p.Status.Nodes[i].Phase == upgradev1alpha1.ConfigNodePending {
			n++
		}
	}

	return n
}

// nextConfigNode: workers first, so a bad config stops before etcd; this
// controller's own node last within its role.
func nextConfigNode(p *upgradev1alpha1.MachineConfigPlan, ownNode string) *upgradev1alpha1.ConfigNodeStatus {
	rank := func(n *upgradev1alpha1.ConfigNodeStatus) string {
		role, own := "0", "0"
		if n.Role == upgradev1alpha1.RoleControlPlane {
			role = "1"
		}

		if n.Name == ownNode {
			own = "1"
		}

		return role + own + n.Name
	}

	var best *upgradev1alpha1.ConfigNodeStatus

	for i := range p.Status.Nodes {
		n := &p.Status.Nodes[i]
		if n.Phase == upgradev1alpha1.ConfigNodePending && (best == nil || rank(n) < rank(best)) {
			best = n
		}
	}

	return best
}

func configCooldownRemaining(p *upgradev1alpha1.MachineConfigPlan, now time.Time) time.Duration {
	var latest time.Time

	for i := range p.Status.Nodes {
		if c := p.Status.Nodes[i].CompletedAt; c != nil && c.After(latest) {
			latest = c.Time
		}
	}

	if latest.IsZero() {
		return 0
	}

	return max(configCooldown(p)-now.Sub(latest), 0)
}

func tryDeadlinePassed(n *upgradev1alpha1.ConfigNodeStatus, now time.Time) bool {
	return n.TryDeadline != nil && !now.Before(n.TryDeadline.Time)
}

func ptrTime(t time.Time) *metav1.Time {
	mt := metav1.NewTime(t)

	return &mt
}

func nodesNotAt(p *upgradev1alpha1.MachineConfigPlan, revision string) string {
	var names []string

	for _, n := range p.Status.Nodes {
		if n.AppliedRevision != revision {
			names = append(names, n.Name)
		}
	}

	return strings.Join(names, ", ")
}

func skippedNodes(p *upgradev1alpha1.MachineConfigPlan) string {
	var parts []string

	for _, n := range p.Status.Nodes {
		if len(n.Skipped) > 0 {
			parts = append(parts, n.Name+" ("+strings.Join(n.Skipped, ", ")+")")
		}
	}

	return strings.Join(parts, "; ")
}
