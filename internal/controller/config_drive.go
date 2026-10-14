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
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
	"github.com/openresearch/talos-lifecycle/pkg/confignode"
	"github.com/openresearch/talos-lifecycle/pkg/configsource"
)

// try-verify-commit:
//
//	Pending -> Trying -> Probing -> HealthChecking -> Committing -> Succeeded
//	Probing / HealthChecking / Committing -> Reverted -> Failed
//
// On any failure the controller sends nothing and waits for the node's own revert.

func (r *MachineConfigPlanReconciler) startConfigNode(
	ctx context.Context,
	plan *upgradev1alpha1.MachineConfigPlan,
	node *upgradev1alpha1.ConfigNodeStatus,
	src *configsource.Source,
) (ctrl.Result, error) {
	poll := ctrl.Result{RequeueAfter: configPollInterval}

	setPlan(plan, upgradev1alpha1.ConfigPlanRollingOut, reasonConfigRollingOut, "starting "+node.Name)

	c, err := r.Dialer.Dial(ctx, node.Address)
	if err != nil {
		node.Message = "could not reach the node: " + err.Error()

		return poll, nil
	}
	defer c.Close() //nolint:errcheck

	persistent, err := confignode.ReadBooted(ctx, c)
	if err != nil {
		node.Message = msgReadFailed + err.Error()

		return poll, nil
	}

	rendered, err := render(src, node, persistent)
	if err != nil {
		r.fail(plan, node, err.Error())

		return poll, nil
	}

	if yield, err := r.yieldToUpgrade(ctx, plan); err != nil {
		return ctrl.Result{}, err
	} else if yield {
		return poll, nil
	}

	timeout := tryTimeout(plan)
	now := time.Now()

	node.TriedHash = confignode.HashBytes(rendered.Bytes)
	node.TryIssuedAt = ptrTime(now)
	node.TryDeadline = ptrTime(now.Add(timeout))
	setNode(node, upgradev1alpha1.ConfigNodeTrying, fmt.Sprintf("trying revision %s, reverts in %s", src.Revision, timeout))
	setPlan(plan, upgradev1alpha1.ConfigPlanRollingOut, reasonConfigRollingOut, "trying on "+node.Name)

	// persisted first: a second TRY would make the tried config the revert target
	if err := persistStatus(ctx, r.Client, plan); err != nil {
		node.TriedHash, node.TryIssuedAt, node.TryDeadline = "", nil, nil
		setNode(node, upgradev1alpha1.ConfigNodePending, "could not record the try intent: "+err.Error())

		return ctrl.Result{}, fmt.Errorf("persist try intent: %w", err)
	}

	details, err := c.ApplyTry(ctx, rendered.Bytes, timeout)

	switch {
	case errors.Is(err, confignode.ErrRejected):
		node.TriedHash, node.TryIssuedAt, node.TryDeadline = "", nil, nil
		r.fail(plan, node, err.Error())
	case err != nil:
		// the TRY may have landed: wait out the revert
		r.beginRevert(plan, node, "try apply outcome unknown: "+err.Error())
	default:
		setNode(node, upgradev1alpha1.ConfigNodeProbing, details)
		r.event(plan, corev1.EventTypeNormal, "ConfigTryApplied",
			fmt.Sprintf("revision %s on %s in try mode, reverts in %s", src.Revision, node.Name, timeout))
	}

	return poll, nil
}

func (r *MachineConfigPlanReconciler) driveConfigNode(
	ctx context.Context,
	plan *upgradev1alpha1.MachineConfigPlan,
	node *upgradev1alpha1.ConfigNodeStatus,
	src *configsource.Source,
) (ctrl.Result, error) {
	setPlan(plan, upgradev1alpha1.ConfigPlanRollingOut, reasonConfigRollingOut, fmt.Sprintf("node %s is %s", node.Name, node.Phase))

	now := time.Now()

	if node.Phase != upgradev1alpha1.ConfigNodeReverted && tryDeadlinePassed(node, now) {
		r.beginRevert(plan, node, "try window expired during "+string(node.Phase))
	}

	switch node.Phase {
	case upgradev1alpha1.ConfigNodeTrying, upgradev1alpha1.ConfigNodeProbing:
		// Trying here means a restart after the persist: the TRY may have landed
		r.probe(ctx, plan, node, now)
	case upgradev1alpha1.ConfigNodeHealthChecking:
		if err := r.checkHealth(ctx, plan, node, now); err != nil {
			return ctrl.Result{}, err
		}
	case upgradev1alpha1.ConfigNodeCommitting:
		r.commit(ctx, plan, node, src)
	case upgradev1alpha1.ConfigNodeReverted:
		r.confirmRevert(ctx, plan, node, now)
	default:
		return ctrl.Result{}, fmt.Errorf("unexpected phase %q on node %s", node.Phase, node.Name)
	}

	return ctrl.Result{RequeueAfter: configPollInterval}, nil
}

// probe asks the node before judging the budget: it bounds the node, not a controller outage.
func (r *MachineConfigPlanReconciler) probe(ctx context.Context, plan *upgradev1alpha1.MachineConfigPlan,
	node *upgradev1alpha1.ConfigNodeStatus, now time.Time,
) {
	active, err := r.slotHash(ctx, node.Address, confignode.SlotActive)

	switch {
	case err == nil && active == node.TriedHash:
		setNode(node, upgradev1alpha1.ConfigNodeHealthChecking, "node runs the config, soaking")
	case now.Sub(node.LastTransition.Time) > probeTimeout(plan):
		r.beginRevert(plan, node, fmt.Sprintf("node did not confirm the config within %s", probeTimeout(plan)))
	case err != nil:
		node.Message = "waiting for the node to answer: " + err.Error()
	default:
		node.Message = "node answers but is not running the tried config"
	}
}

func (r *MachineConfigPlanReconciler) checkHealth(ctx context.Context, plan *upgradev1alpha1.MachineConfigPlan,
	node *upgradev1alpha1.ConfigNodeStatus, now time.Time,
) error {
	issued := node.LastTransition.Time
	if node.TryIssuedAt != nil {
		issued = node.TryIssuedAt.Time
	}

	soakEnd := issued.Add(soakTime(plan))
	if soakEnd.After(now) {
		node.Message = "soaking until " + soakEnd.UTC().Format(time.TimeOnly) + " UTC, then the health check that decides the commit"

		return nil
	}

	start := node.LastTransition.Time
	if soakEnd.After(start) {
		start = soakEnd
	}

	healthy, reason, err := r.clusterHealthy(ctx)

	switch {
	case err == nil && healthy:
		setNode(node, upgradev1alpha1.ConfigNodeCommitting, "cluster healthy, committing")
	case now.Sub(start) > healthTimeout(plan):
		r.beginRevert(plan, node, fmt.Sprintf("cluster health did not recover within %s", healthTimeout(plan)))
	case err != nil:
		return err
	default:
		node.Message = "waiting for cluster health: " + reason
	}

	return nil
}

func (r *MachineConfigPlanReconciler) commit(ctx context.Context, plan *upgradev1alpha1.MachineConfigPlan,
	node *upgradev1alpha1.ConfigNodeStatus, src *configsource.Source,
) {
	c, err := r.Dialer.Dial(ctx, node.Address)
	if err != nil {
		node.Message = "could not reach the node to commit: " + err.Error()

		return
	}
	defer c.Close() //nolint:errcheck

	// TRY never writes the persistent slot, so this is the base the try was rendered on
	persistent, err := confignode.ReadBooted(ctx, c)
	if err != nil {
		node.Message = "could not read the persistent slot: " + err.Error()

		return
	}

	rendered, err := render(src, node, persistent)
	if err != nil {
		r.beginRevert(plan, node, "source no longer renders: "+err.Error())

		return
	}

	if confignode.HashBytes(rendered.Bytes) != node.TriedHash {
		r.beginRevert(plan, node, "source or persistent config changed during verification")

		return
	}

	active, err := hashOf(ctx, c, confignode.SlotActive)
	if err != nil {
		node.Message = "could not confirm the running config: " + err.Error()

		return
	}

	if active != node.TriedHash {
		r.beginRevert(plan, node, "node reverted before the commit")

		return
	}

	if _, err := c.ApplyCommit(ctx, rendered.Bytes); err != nil {
		if errors.Is(err, confignode.ErrRejected) {
			// Talos cancels the try timer before it validates: no revert is coming
			node.TryIssuedAt, node.TryDeadline = nil, nil
			r.fail(plan, node, "commit rejected ("+err.Error()+"); the try timer is cancelled, so the node runs "+
				"the tried config, not persisted, until its next reboot; reboot it before acknowledging")
		} else {
			node.Message = "commit failed, retrying: " + err.Error()
		}

		return
	}

	committed, err := hashOf(ctx, c, confignode.SlotPersistent)
	if err != nil {
		node.Message = "committed, confirming the persistent slot: " + err.Error()

		return
	}

	if committed != node.TriedHash {
		r.fail(plan, node, "commit reported success but the persistent slot differs")

		return
	}

	node.AppliedRevision = src.Revision
	markCommitted(node, "config committed and cluster healthy")
	r.event(plan, corev1.EventTypeNormal, "ConfigCommitted", fmt.Sprintf("revision %s on %s", src.Revision, node.Name))
}

func markCommitted(node *upgradev1alpha1.ConfigNodeStatus, msg string) {
	metricConfigApplies.WithLabelValues("committed").Inc()

	node.ObservedHash = node.TriedHash
	node.Diff = ""
	node.TryIssuedAt, node.TryDeadline = nil, nil
	node.CompletedAt = ptrTime(time.Now())
	setNode(node, upgradev1alpha1.ConfigNodeSucceeded, msg)
}

func (r *MachineConfigPlanReconciler) beginRevert(plan *upgradev1alpha1.MachineConfigPlan,
	node *upgradev1alpha1.ConfigNodeStatus, reason string,
) {
	node.RevertReason = reason
	setNode(node, upgradev1alpha1.ConfigNodeReverted, reason)
	r.event(plan, corev1.EventTypeWarning, "ConfigReverting", fmt.Sprintf("%s: %s; waiting for its own revert", node.Name, reason))
}

func (r *MachineConfigPlanReconciler) confirmRevert(ctx context.Context, plan *upgradev1alpha1.MachineConfigPlan,
	node *upgradev1alpha1.ConfigNodeStatus, now time.Time,
) {
	deadline := node.LastTransition.Add(tryTimeout(plan))
	if node.TryDeadline != nil {
		deadline = node.TryDeadline.Time
	}

	if now.Before(deadline) {
		node.Message = "waiting for the node to revert on its own"

		return
	}

	late := now.After(deadline.Add(revertGrace))

	var (
		active, persistent string
		errA, errP         error
	)

	if c, err := r.Dialer.Dial(ctx, node.Address); err != nil {
		errA = err
	} else {
		defer c.Close() //nolint:errcheck

		active, errA = hashOf(ctx, c, confignode.SlotActive)
		persistent, errP = hashOf(ctx, c, confignode.SlotPersistent)
	}

	bootedOnly := errors.Is(errP, confignode.ErrNotPersisted)
	if bootedOnly {
		errP = nil
	}

	switch {
	case errA != nil || errP != nil:
		if late {
			r.fail(plan, node, fmt.Sprintf("could not confirm the revert (%v); needs human eyes", errors.Join(errA, errP)))
		} else {
			node.Message = "waiting to confirm the revert"
		}
	case !bootedOnly && persistent == node.TriedHash:
		// only the commit persists the tried bytes: it landed, its status write did not
		markCommitted(node, "config committed; found on the node after the try deadline")
		r.event(plan, corev1.EventTypeNormal, "ConfigCommitted", node.Name+": the commit had landed, its status write was lost")
	case active == node.TriedHash || (!bootedOnly && active != persistent):
		if late {
			r.fail(plan, node, "node did not revert: it still runs the tried config or differs from persistent; needs human eyes")
		} else {
			node.Message = "revert not visible yet"
		}
	default:
		node.ObservedHash = active
		node.TryIssuedAt, node.TryDeadline = nil, nil
		r.fail(plan, node, "node reverted to its last good config after: "+node.RevertReason)
	}
}

func (r *MachineConfigPlanReconciler) fail(plan *upgradev1alpha1.MachineConfigPlan, node *upgradev1alpha1.ConfigNodeStatus, msg string) {
	metricConfigApplies.WithLabelValues("failed").Inc()

	setNode(node, upgradev1alpha1.ConfigNodeFailed, msg)
	setPlan(plan, upgradev1alpha1.ConfigPlanFrozen, reasonConfigNodeFailed, fmt.Sprintf("node %s failed: %s", node.Name, msg))
	r.event(plan, corev1.EventTypeWarning, "ConfigNodeFailed", fmt.Sprintf("%s: %s", node.Name, msg))
}

func (r *MachineConfigPlanReconciler) slotHash(ctx context.Context, address string, slot confignode.Slot) (string, error) {
	c, err := r.Dialer.Dial(ctx, address)
	if err != nil {
		return "", err
	}
	defer c.Close() //nolint:errcheck

	return hashOf(ctx, c, slot)
}

func hashOf(ctx context.Context, c confignode.Client, slot confignode.Slot) (string, error) {
	cfg, err := c.ReadSlot(ctx, slot)
	if err != nil {
		return "", err
	}

	return confignode.Hash(cfg)
}
