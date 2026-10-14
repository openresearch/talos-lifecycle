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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/config"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
	"github.com/openresearch/talos-lifecycle/internal/talosutil"
	"github.com/openresearch/talos-lifecycle/pkg/configdiff"
	"github.com/openresearch/talos-lifecycle/pkg/confignode"
	"github.com/openresearch/talos-lifecycle/pkg/configrender"
	"github.com/openresearch/talos-lifecycle/pkg/configsource"
)

// RBAC is hand-written in config/overlays/config-controller: kubebuilder markers
// here would land in the upgrade controller's role.
type MachineConfigPlanReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	Dialer confignode.Dialer
	// reads the Git Secret uncached, so no Secret informer starts
	APIReader   client.Reader
	HealthCheck func(ctx context.Context) (healthy bool, reason string, err error)
	// this node goes last
	NodeName  string
	SourceFor func(*upgradev1alpha1.MachineConfigPlan) (configsource.Fetcher, error)

	// a node in flight finishes on the last tree while Git is unreachable
	lastSource *configsource.Source
}

func (r *MachineConfigPlanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Name != upgradev1alpha1.MachineConfigPlanName {
		return ctrl.Result{}, nil
	}

	var plan upgradev1alpha1.MachineConfigPlan
	if err := r.Get(ctx, req.NamespacedName, &plan); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	original := plan.DeepCopy()
	result, err := r.reconcilePlan(ctx, &plan)
	updateConfigMetrics(&plan)

	// even after an error: an issued apply must stay recorded
	if !equality.Semantic.DeepEqual(original.Status, plan.Status) {
		if statusErr := r.Status().Update(ctx, &plan); statusErr != nil && !apierrors.IsConflict(statusErr) {
			if err == nil {
				return ctrl.Result{}, fmt.Errorf("update status: %w", statusErr)
			}

			log.FromContext(ctx).Error(statusErr, "could not persist status")
		}
	}

	return result, err
}

func (r *MachineConfigPlanReconciler) reconcilePlan(ctx context.Context, plan *upgradev1alpha1.MachineConfigPlan) (ctrl.Result, error) {
	idle := ctrl.Result{RequeueAfter: configIdleInterval}
	poll := ctrl.Result{RequeueAfter: configPollInterval}

	// before the freeze check, or the plan could never leave Frozen
	if handled, err := r.handleUnfreeze(ctx, plan); err != nil || handled {
		return poll, err
	}

	if err := r.observeNodes(ctx, plan); err != nil {
		return ctrl.Result{}, err
	}

	inFlight := findNode(plan, func(n *upgradev1alpha1.ConfigNodeStatus) bool { return n.Phase.InFlight() })

	src, err := r.fetchSource(ctx, plan)

	switch {
	case err == nil:
		r.lastSource = src
	case inFlight != nil && r.lastSource != nil:
		log.FromContext(ctx).Info("source unreachable, finishing the node in flight on the last tree",
			"node", inFlight.Name, "error", err.Error())

		src = r.lastSource
	default:
		setPlan(plan, upgradev1alpha1.ConfigPlanWaiting, reasonConfigSourceError, err.Error())

		return idle, nil
	}

	plan.Status.SourceRevision = src.Revision
	plan.Status.SourceCommit = src.Commit

	if err := r.refreshNodes(ctx, plan, src); err != nil {
		setPlan(plan, upgradev1alpha1.ConfigPlanFrozen, reasonConfigInvalid, err.Error())

		return idle, nil
	}

	if inFlight != nil {
		return r.driveConfigNode(ctx, plan, inFlight, src)
	}

	if failed := findNode(plan, func(n *upgradev1alpha1.ConfigNodeStatus) bool {
		return n.Phase == upgradev1alpha1.ConfigNodeFailed
	}); failed != nil {
		setPlan(plan, upgradev1alpha1.ConfigPlanFrozen, reasonConfigNodeFailed, fmt.Sprintf(
			"node %s failed: %s; acknowledge with the %s annotation", failed.Name, failed.Message, upgradev1alpha1.UnfreezeAnnotation))

		return idle, nil
	}

	pending := countPending(plan)
	now := time.Now()

	switch {
	case pending == 0:
		if unread := nodesNotAt(plan, src.Revision); unread != "" {
			setPlan(plan, upgradev1alpha1.ConfigPlanWaiting, reasonConfigClusterUnready, fmt.Sprintf(
				"no node differs from revision %s, but %s could not be read against it", src.Revision, unread))

			return idle, nil
		}

		plan.Status.AppliedRevision = src.Revision

		if skipped := skippedNodes(plan); skipped != "" {
			setPlan(plan, upgradev1alpha1.ConfigPlanIdle, reasonConfigRebootOnlySkipped, fmt.Sprintf(
				"all %d nodes match revision %s except reboot-only fields, not applied: %s",
				len(plan.Status.Nodes), src.Revision, skipped))

			return idle, nil
		}

		setPlan(plan, upgradev1alpha1.ConfigPlanIdle, reasonConfigUpToDate,
			fmt.Sprintf("all %d nodes match revision %s", len(plan.Status.Nodes), src.Revision))

		return idle, nil

	case configMode(plan) == upgradev1alpha1.ModeDiffOnly:
		setPlan(plan, upgradev1alpha1.ConfigPlanDiffing, reasonConfigDiffOnly,
			fmt.Sprintf("%d node(s) differ from revision %s; DiffOnly applies nothing", pending, src.Revision))

		return idle, nil

	case plan.Spec.Paused:
		setPlan(plan, upgradev1alpha1.ConfigPlanWaiting, reasonConfigPaused, fmt.Sprintf("%d node(s) pending, paused", pending))

		return idle, nil

	case !tryBudgetSafe(plan):
		setPlan(plan, upgradev1alpha1.ConfigPlanFrozen, reasonConfigInvalid, fmt.Sprintf(
			"tryTimeout (%s) must exceed probe + soak + health (%s) plus %s, or the node reverts before the commit",
			tryTimeout(plan), verifyBudget(plan), commitHeadroom))

		return idle, nil
	}

	if gate, _ := windowGate(plan.Spec.Window, now); !gate.passed() {
		setPlan(plan, upgradev1alpha1.ConfigPlanWaiting, reasonConfigWindow, gate.reason)

		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	if wait := configCooldownRemaining(plan, now); wait > 0 {
		setPlan(plan, upgradev1alpha1.ConfigPlanWaiting, reasonConfigCooldown,
			"cooling down until "+now.Add(wait).UTC().Format(time.TimeOnly)+" UTC")

		return ctrl.Result{RequeueAfter: wait}, nil
	}

	if yield, err := r.yieldToUpgrade(ctx, plan); err != nil {
		return ctrl.Result{}, err
	} else if yield {
		return poll, nil
	}

	// unhealthy before the change: do not blame the change for it
	healthy, reason, err := r.clusterHealthy(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !healthy {
		setPlan(plan, upgradev1alpha1.ConfigPlanWaiting, reasonConfigClusterUnready, "cluster is not healthy: "+reason)

		return poll, nil
	}

	next := nextConfigNode(plan, r.NodeName)
	if next == nil {
		return idle, nil
	}

	return r.startConfigNode(ctx, plan, next, src)
}

func (r *MachineConfigPlanReconciler) observeNodes(ctx context.Context, plan *upgradev1alpha1.MachineConfigPlan) error {
	live, err := clusterNodes(ctx, r.Client)
	if err != nil {
		return err
	}

	previous := map[string]upgradev1alpha1.ConfigNodeStatus{}
	for _, n := range plan.Status.Nodes {
		previous[n.Name] = n
	}

	observed := make([]upgradev1alpha1.ConfigNodeStatus, 0, len(live))

	for _, n := range live {
		if n.Address == "" {
			continue
		}

		status, ok := previous[n.Name]
		if !ok {
			status = upgradev1alpha1.ConfigNodeStatus{Name: n.Name, Phase: upgradev1alpha1.ConfigNodePending, Message: msgNotObserved}
		}

		delete(previous, n.Name)

		status.Address, status.Role = n.Address, n.Role
		observed = append(observed, status)
	}

	for _, gone := range previous {
		if gone.Phase.InFlight() || gone.Phase == upgradev1alpha1.ConfigNodeFailed {
			observed = append(observed, gone)
		}
	}

	sort.Slice(observed, func(i, j int) bool { return observed[i].Name < observed[j].Name })
	plan.Status.Nodes = observed

	return nil
}

func (r *MachineConfigPlanReconciler) fetchSource(ctx context.Context, plan *upgradev1alpha1.MachineConfigPlan) (*configsource.Source, error) {
	var (
		fetcher configsource.Fetcher
		err     error
	)

	switch src := plan.Spec.Source; {
	case r.SourceFor != nil:
		fetcher, err = r.SourceFor(plan)
	case src.Git != nil:
		fetcher, err = r.gitFetcher(ctx, src.Git)
	case src.Path != "":
		fetcher = configsource.DirFetcher{Path: src.Path}
	default:
		err = errors.New("no source configured: set spec.source.git or spec.source.path")
	}

	if err != nil {
		return nil, err
	}

	return fetcher.Fetch(ctx)
}

func (r *MachineConfigPlanReconciler) gitFetcher(ctx context.Context, src *upgradev1alpha1.GitSource) (configsource.Fetcher, error) {
	ns := src.SecretRef.Namespace
	if ns == "" {
		var err error
		if ns, err = talosutil.Namespace(); err != nil {
			return nil, fmt.Errorf("git secret has no namespace: %w", err)
		}
	}

	var secret corev1.Secret
	if err := uncached(r.APIReader, r.Client).Get(ctx, client.ObjectKey{Namespace: ns, Name: src.SecretRef.Name}, &secret); err != nil {
		return nil, fmt.Errorf("get git secret %s/%s: %w", ns, src.SecretRef.Name, err)
	}

	return configsource.GitFetcher{
		URL:        src.URL,
		Ref:        src.Ref,
		Path:       src.Path,
		Identity:   secret.Data["identity"],
		KnownHosts: secret.Data["known_hosts"],
	}, nil
}

func (r *MachineConfigPlanReconciler) refreshNodes(ctx context.Context, plan *upgradev1alpha1.MachineConfigPlan, src *configsource.Source) error {
	for i := range plan.Status.Nodes {
		node := &plan.Status.Nodes[i]

		// re-rendering an in-flight node would move the bytes its commit must match
		if node.Phase.InFlight() || node.Phase == upgradev1alpha1.ConfigNodeFailed {
			continue
		}

		persistent, err := r.readBooted(ctx, node.Address)
		if err != nil {
			node.Message = msgReadFailed + err.Error()

			continue
		}

		rendered, err := render(src, node, persistent)
		if err != nil {
			return err
		}

		observed, err := confignode.Hash(persistent)
		if err != nil {
			return err
		}

		node.DesiredHash = confignode.HashBytes(rendered.Bytes)
		node.ObservedHash = observed
		node.Warnings = rendered.Warnings

		if !slices.Equal(node.Skipped, rendered.Skipped) && len(rendered.Skipped) > 0 {
			r.event(plan, corev1.EventTypeWarning, reasonConfigRebootOnlySkipped, fmt.Sprintf(
				"%s: not applied, takes effect only after a reboot: %s", node.Name, strings.Join(rendered.Skipped, ", ")))
		}

		node.Skipped = rendered.Skipped

		if observed == node.DesiredHash {
			node.Diff = ""
			node.AppliedRevision = src.Revision

			if node.Phase != upgradev1alpha1.ConfigNodeSucceeded {
				setNode(node, upgradev1alpha1.ConfigNodeSucceeded, "config matches the source")
			} else if strings.HasPrefix(node.Message, msgReadFailed) {
				node.Message = "config matches the source"
			}

			continue
		}

		node.Diff = diff(persistent, rendered.Config)

		differs := msgDiffers + src.Revision
		if node.Phase != upgradev1alpha1.ConfigNodePending {
			setNode(node, upgradev1alpha1.ConfigNodePending, differs)
		} else if node.Message == "" || node.Message == msgNotObserved ||
			strings.HasPrefix(node.Message, msgReadFailed) || strings.HasPrefix(node.Message, msgDiffers) {
			node.Message = differs
		}
	}

	return nil
}

func render(src *configsource.Source, node *upgradev1alpha1.ConfigNodeStatus, persistent config.Provider) (*configrender.Result, error) {
	return configrender.Render(src.FS, configrender.Node{Name: node.Name, Role: node.Role}, persistent)
}

func diff(from, to config.Provider) string {
	report, err := configdiff.Diff(from, to)
	if err != nil {
		return "(diff unavailable: " + err.Error() + ")"
	}

	out := report.String()
	if len(out) > maxDiffBytes {
		out = out[:maxDiffBytes] + "\n... diff truncated ...\n"
	}

	return out
}

func (r *MachineConfigPlanReconciler) readBooted(ctx context.Context, address string) (config.Provider, error) {
	c, err := r.Dialer.Dial(ctx, address)
	if err != nil {
		return nil, err
	}
	defer c.Close() //nolint:errcheck

	return confignode.ReadBooted(ctx, c)
}

// yieldToUpgrade is called again right before a try is persisted: the health gate can take a minute.
func (r *MachineConfigPlanReconciler) yieldToUpgrade(ctx context.Context, plan *upgradev1alpha1.MachineConfigPlan) (bool, error) {
	busy, err := upgradeInFlight(ctx, uncached(r.APIReader, r.Client))
	if err != nil || busy == "" {
		return false, err
	}

	setPlan(plan, upgradev1alpha1.ConfigPlanWaiting, reasonConfigUpgradeInFlight, "upgrade loop is driving "+busy)

	return true, nil
}

func (r *MachineConfigPlanReconciler) clusterHealthy(ctx context.Context) (bool, string, error) {
	if r.HealthCheck == nil {
		return true, "", nil
	}

	return r.HealthCheck(ctx)
}

// handleUnfreeze writes status before removing the annotation; the other order
// loses the reset to a conflict.
func (r *MachineConfigPlanReconciler) handleUnfreeze(ctx context.Context, plan *upgradev1alpha1.MachineConfigPlan) (bool, error) {
	target, ok := plan.Annotations[upgradev1alpha1.UnfreezeAnnotation]
	if !ok {
		return false, nil
	}

	target = strings.TrimSpace(target)
	reset := 0

	for i := range plan.Status.Nodes {
		node := &plan.Status.Nodes[i]
		if node.Phase != upgradev1alpha1.ConfigNodeFailed || (target != unfreezeAll && node.Name != target) {
			continue
		}

		setNode(node, upgradev1alpha1.ConfigNodePending, "failure acknowledged")
		node.TriedHash, node.TryIssuedAt, node.TryDeadline = "", nil, nil
		reset++
	}

	setPlan(plan, upgradev1alpha1.ConfigPlanWaiting, reasonConfigUnfrozen, fmt.Sprintf("%d node(s) reset by the unfreeze annotation", reset))

	if err := persistStatus(ctx, r.Client, plan); err != nil {
		return false, fmt.Errorf("persist unfreeze: %w", err)
	}

	if err := removeAnnotation(ctx, r.Client, plan, upgradev1alpha1.UnfreezeAnnotation); err != nil {
		return false, err
	}

	r.event(plan, corev1.EventTypeNormal, "ConfigUnfrozen", fmt.Sprintf("%q acknowledged, %d node(s) reset", target, reset))

	return true, nil
}

func (r *MachineConfigPlanReconciler) event(plan *upgradev1alpha1.MachineConfigPlan, eventType, reason, msg string) {
	emit(r.Recorder, plan, eventType, reason, msg)
}

func (r *MachineConfigPlanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	registerConfigMetrics()

	if r.Dialer == nil {
		r.Dialer = confignode.TalosDialer{}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&upgradev1alpha1.MachineConfigPlan{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("machineconfigplan").
		Complete(r)
}
