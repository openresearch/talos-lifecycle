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
	"fmt"
	"sort"
	"time"

	"golang.org/x/mod/semver"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

const (
	defaultUpgradeTimeout     = 30 * time.Minute
	defaultVerifyTimeout      = 15 * time.Minute
	defaultHealthCheckTimeout = 15 * time.Minute
	defaultRollbackTimeout    = 20 * time.Minute
	defaultCooldown           = 60 * time.Minute

	healthAttemptTimeout = 60 * time.Second

	pollInterval         = 10 * time.Second
	idleInterval         = 5 * time.Minute
	clusterCheckInterval = time.Minute
	// longer than the 15 minutes of the PlanBlocked alert, so a retry does not reset it
	upgradeRefusedRetry = 30 * time.Minute
)

var blockedReasons = map[string]bool{
	reasonUnsupportedTalos:      true,
	reasonUnsupportedKubernetes: true,
	reasonIncompatibleVersions:  true,
	reasonUpgradeRefused:        true,
}

const (
	reasonUpToDate              = "UpToDate"
	reasonTalosUpgrade          = "TalosUpgrade"
	reasonKubernetesUpgrade     = "KubernetesUpgrade"
	reasonWaitingForWindow      = "WaitingForWindow"
	reasonCoolingDown           = "CoolingDown"
	reasonWaitingForCluster     = "WaitingForCluster"
	reasonEtcdSnapshot          = "WaitingForEtcdSnapshot"
	reasonUnsupportedTalos      = "UnsupportedTalosTarget"
	reasonUnsupportedKubernetes = "UnsupportedKubernetesPath"
	reasonIncompatibleVersions  = "IncompatibleVersions"
	reasonWaitingForEtcd        = "WaitingForEtcdQuorum"
	reasonWaitingForEviction    = "WaitingForEviction"
	reasonWaitingForConfig      = "WaitingForConfig"
	reasonUpgradeRefused        = "UpgradeRefused"
	reasonUpgradeFailed         = "UpgradeFailed"
	reasonNodeHeld              = "NodeHeld"
)

type UpgradePlanReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// reads the MachineConfigPlan uncached: its CRD may not be installed
	APIReader client.Reader

	lastSkippedKubernetesTarget string
	lastSkippedTalosTarget      string

	talos talosAPI
}

func (r *UpgradePlanReconciler) event(plan *upgradev1alpha1.UpgradePlan, eventType, reason, message string) {
	emit(r.Recorder, plan, eventType, reason, message)
}

// +kubebuilder:rbac:groups=talos.openresearch.com,resources=upgradeplans,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=talos.openresearch.com,resources=upgradeplans,verbs=update;patch
// +kubebuilder:rbac:groups=talos.openresearch.com,resources=upgradeplans/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=talos.openresearch.com,resources=machineconfigplans,verbs=get
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",namespace=system,resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *UpgradePlanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	if req.Name != upgradev1alpha1.UpgradePlanName {
		log.Info("ignoring UpgradePlan with unexpected name, only one named 'cluster' is reconciled", "name", req.Name)

		return ctrl.Result{}, nil
	}

	var plan upgradev1alpha1.UpgradePlan
	if err := r.Get(ctx, req.NamespacedName, &plan); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	observedStatus := plan.Status.DeepCopy()

	if handled, err := r.handleUnfreeze(ctx, &plan); err != nil {
		return ctrl.Result{}, err
	} else if handled {
		return ctrl.Result{}, nil
	}

	if err := r.observeNodes(ctx, &plan); err != nil {
		return ctrl.Result{}, fmt.Errorf("observe cluster: %w", err)
	}

	if plan.Spec.Channel == upgradev1alpha1.ChannelLatest {
		bumped, err := r.autoBumpTargets(ctx, &plan)
		if err != nil {
			return ctrl.Result{}, err
		}

		if bumped {
			return ctrl.Result{}, nil
		}
	}

	result, err := r.step(ctx, &plan)
	if err != nil {
		return ctrl.Result{}, err
	}

	updateNodeMetrics(&plan)

	if equality.Semantic.DeepEqual(*observedStatus, plan.Status) {
		return result, nil
	}

	if err := r.Status().Update(ctx, &plan); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}

		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}

	return result, nil
}

func (r *UpgradePlanReconciler) step(ctx context.Context, plan *upgradev1alpha1.UpgradePlan) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	if node := inFlightNode(plan); node != nil {
		r.setPhase(plan, upgradev1alpha1.PlanPhaseUpgrading, reasonTalosUpgrade,
			fmt.Sprintf("upgrading node %s to %s", node.Name, nodeTarget(plan, node)))

		return r.driveNodeUpgrade(ctx, plan, node)
	}

	if plan.Status.Kubernetes.Phase == upgradev1alpha1.NodePhaseUpgrading {
		r.setPhase(plan, upgradev1alpha1.PlanPhaseUpgrading, reasonKubernetesUpgrade, "resuming kubernetes upgrade")

		return r.driveKubernetesUpgrade(ctx, plan)
	}

	if plan.Status.Kubernetes.Phase == upgradev1alpha1.NodePhaseHealthChecking {
		r.setPhase(plan, upgradev1alpha1.PlanPhaseUpgrading, reasonKubernetesUpgrade,
			"verifying cluster health after kubernetes upgrade")

		return r.driveKubernetesHealthCheck(ctx, plan)
	}

	if name, msg := failedTarget(plan); name != "" {
		log.Info("plan frozen: previous upgrade failed, operator intervention required",
			"target", name, "reason", msg)
		r.setPhase(plan, upgradev1alpha1.PlanPhaseFrozen, reasonUpgradeFailed,
			fmt.Sprintf("%s failed: %s; resolve, then acknowledge with the %s annotation",
				name, msg, upgradev1alpha1.UnfreezeAnnotation))

		return ctrl.Result{RequeueAfter: idleInterval}, nil
	}

	if name := unknownVersionNode(plan); plan.Spec.TalosVersion != "" && name != "" {
		r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonWaitingForCluster,
			fmt.Sprintf("talos version of node %s is unknown (API unreachable?)", name))

		return ctrl.Result{RequeueAfter: clusterCheckInterval}, nil
	}

	if plan.Spec.TalosVersion != "" && !talosTargetSupported(plan.Spec.TalosVersion) {
		r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonUnsupportedTalos,
			fmt.Sprintf("talos %s is newer than this controller build supports (max %s.x); "+
				"update the controller to upgrade further",
				plan.Spec.TalosVersion, maxSupportedTalosMinor()))

		return ctrl.Result{RequeueAfter: idleInterval}, nil
	}

	next := nextTalosNode(plan)

	if next == nil && !kubernetesUpgradePending(plan) {
		if held := heldNodes(plan, plan.Spec.TalosVersion); plan.Spec.TalosVersion != "" && len(held) > 0 {
			r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonNodeHeld, heldMessage(held))

			return ctrl.Result{RequeueAfter: idleInterval}, nil
		}

		// the targets are a floor: nodes above them are left alone, never downgraded
		r.setPhase(plan, upgradev1alpha1.PlanPhaseIdle, reasonUpToDate, "every node runs the desired versions or newer")

		return ctrl.Result{RequeueAfter: idleInterval}, nil
	}

	next = orderTalosAndKubernetes(plan, next)
	if why := compatibilityGate(plan, next); why != "" {
		r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonIncompatibleVersions, why)

		return ctrl.Result{RequeueAfter: idleInterval}, nil
	}

	return r.startNext(ctx, plan, next)
}

func (r *UpgradePlanReconciler) startNext(
	ctx context.Context,
	plan *upgradev1alpha1.UpgradePlan,
	next *upgradev1alpha1.NodeStatus,
) (ctrl.Result, error) {
	now := time.Now()
	if next != nil {
		if why, wait := refusedUpgrade(next, plan.Spec.TalosVersion, now); why != "" {
			r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonUpgradeRefused, why)

			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}

	if gate, wait := windowGate(plan.Spec.Window, now); !gate.passed() {
		r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonWaitingForWindow, gate.reason)

		return ctrl.Result{RequeueAfter: wait}, nil
	}

	if wait := cooldownRemaining(now, plan); wait > 0 {
		r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonCoolingDown,
			"cooling down until "+now.Add(wait).UTC().Format(time.TimeOnly)+" UTC")

		return ctrl.Result{RequeueAfter: wait}, nil
	}

	if yield, err := r.yieldToConfig(ctx, plan); err != nil {
		return ctrl.Result{}, err
	} else if yield {
		return ctrl.Result{RequeueAfter: clusterCheckInterval}, nil
	}

	if gate, err := r.nodesReady(ctx); err != nil {
		return ctrl.Result{}, err
	} else if !gate.passed() {
		r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonWaitingForCluster, gate.reason)

		return ctrl.Result{RequeueAfter: clusterCheckInterval}, nil
	}

	if next != nil {
		if next.Role == upgradev1alpha1.RoleControlPlane {
			if result, err := r.controlPlaneReady(ctx, plan); err != nil {
				return ctrl.Result{}, err
			} else if !result.passed() {
				r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonWaitingForEtcd, result.reason)

				return ctrl.Result{RequeueAfter: clusterCheckInterval}, nil
			}

			if reason := r.ensureEtcdSnapshot(ctx, plan, "pre-talos-"+next.Name); reason != "" {
				r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonEtcdSnapshot, reason)

				return ctrl.Result{RequeueAfter: idleInterval}, nil
			}
		}

		r.setPhase(plan, upgradev1alpha1.PlanPhaseUpgrading, reasonTalosUpgrade,
			fmt.Sprintf("upgrading node %s to %s", next.Name, plan.Spec.TalosVersion))

		return r.startNodeUpgrade(ctx, plan, next)
	}

	// UpgradeKubelet covers every node, held ones too
	if held := heldNodes(plan, ""); len(held) > 0 {
		r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonNodeHeld, heldMessage(held))

		return ctrl.Result{RequeueAfter: clusterCheckInterval}, nil
	}

	// k8s.Upgrade would refuse the path only after the plan shows Upgrading
	if from, to := plan.Status.Kubernetes.CurrentVersion, plan.Spec.KubernetesVersion; !kubernetesPathSupported(from, to) {
		r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonUnsupportedKubernetes, unsupportedKubernetesPath(from, to))

		return ctrl.Result{RequeueAfter: idleInterval}, nil
	}

	if result, err := r.controlPlaneReady(ctx, plan); err != nil {
		return ctrl.Result{}, err
	} else if !result.passed() {
		r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonWaitingForEtcd, result.reason)

		return ctrl.Result{RequeueAfter: clusterCheckInterval}, nil
	}

	if reason := r.ensureEtcdSnapshot(ctx, plan, "pre-kubernetes"); reason != "" {
		r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonEtcdSnapshot, reason)

		return ctrl.Result{RequeueAfter: idleInterval}, nil
	}

	r.setPhase(plan, upgradev1alpha1.PlanPhaseUpgrading, reasonKubernetesUpgrade,
		fmt.Sprintf("upgrading kubernetes to %s", plan.Spec.KubernetesVersion))

	return r.startKubernetesUpgrade(ctx, plan)
}

// yieldToConfig is called again right before a start is persisted: the gates can take minutes.
func (r *UpgradePlanReconciler) yieldToConfig(ctx context.Context, plan *upgradev1alpha1.UpgradePlan) (bool, error) {
	node, err := configInFlight(ctx, uncached(r.APIReader, r.Client))
	if err != nil || node == "" {
		return false, err
	}

	r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonWaitingForConfig,
		fmt.Sprintf("config loop is applying to node %s", node))

	return true, nil
}

func (r *UpgradePlanReconciler) observeNodes(ctx context.Context, plan *upgradev1alpha1.UpgradePlan) error {
	log := log.FromContext(ctx)

	live, err := clusterNodes(ctx, r.Client)
	if err != nil {
		return err
	}

	previous := make(map[string]upgradev1alpha1.NodeStatus, len(plan.Status.Nodes))
	for _, n := range plan.Status.Nodes {
		previous[n.Name] = n
	}

	inFlight := inFlightNode(plan) != nil || plan.Status.Kubernetes.Phase.InFlight()
	lowestKubelet := ""

	nodes := make([]upgradev1alpha1.NodeStatus, 0, len(live))

	for _, k8sNode := range live {
		status := previous[k8sNode.Name]
		status.Name, status.Role, status.Address = k8sNode.Name, k8sNode.Role, k8sNode.Address
		status.Held = k8sNode.Held

		if !inFlight && status.Address != "" {
			version, err := r.nodes().Version(ctx, status.Address)
			if err != nil {
				log.V(1).Info("could not read talos version", "node", k8sNode.Name, "error", err.Error())
			} else {
				status.CurrentVersion = version
			}
		}

		syncNodePhase(&status, plan.Spec.TalosVersion)

		if k8sNode.Kubelet != "" && (lowestKubelet == "" || semver.Compare(k8sNode.Kubelet, lowestKubelet) < 0) {
			lowestKubelet = k8sNode.Kubelet
		}

		nodes = append(nodes, status)

		delete(previous, k8sNode.Name)
	}

	for _, gone := range previous {
		if gone.Phase.InFlight() || gone.Phase == upgradev1alpha1.NodePhaseFailed {
			nodes = append(nodes, gone)
		}
	}

	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	plan.Status.Nodes = nodes

	plan.Status.Kubernetes.CurrentVersion = lowestKubelet
	syncKubernetesPhase(&plan.Status.Kubernetes, plan.Spec.KubernetesVersion)

	return nil
}

// phaseDecision: Failed stays Failed even at the target; only an unfreeze clears it.
func phaseDecision(current upgradev1alpha1.NodePhase, atTarget bool) (upgradev1alpha1.NodePhase, string) {
	if current.InFlight() || current == upgradev1alpha1.NodePhaseFailed {
		return "", ""
	}

	switch {
	case current == upgradev1alpha1.NodePhaseSucceeded && !atTarget:
		return upgradev1alpha1.NodePhasePending, "new target version"
	case current != upgradev1alpha1.NodePhaseSucceeded && atTarget:
		return upgradev1alpha1.NodePhaseSucceeded, "already at target version"
	case current == "":
		return upgradev1alpha1.NodePhasePending, ""
	}

	return "", ""
}

func syncNodePhase(n *upgradev1alpha1.NodeStatus, target string) {
	atTarget := target != "" && n.CurrentVersion != "" && semver.Compare(n.CurrentVersion, target) >= 0
	if phase, message := phaseDecision(n.Phase, atTarget); phase != "" {
		transition(n, phase, message)
	}
}

func syncKubernetesPhase(k *upgradev1alpha1.KubernetesStatus, target string) {
	atTarget := target != "" && k.CurrentVersion != "" && semver.Compare(k.CurrentVersion, target) >= 0
	if phase, message := phaseDecision(k.Phase, atTarget); phase != "" {
		transitionKubernetes(k, phase, message)
	}
}

func transition(n *upgradev1alpha1.NodeStatus, phase upgradev1alpha1.NodePhase, message string) {
	n.Phase = phase
	n.Message = message
	n.LastTransition = metav1.Now()
}

func transitionKubernetes(k *upgradev1alpha1.KubernetesStatus, phase upgradev1alpha1.NodePhase, message string) {
	k.Phase = phase
	k.Message = message
	k.LastTransition = metav1.Now()
}

func inFlightNode(plan *upgradev1alpha1.UpgradePlan) *upgradev1alpha1.NodeStatus {
	for i := range plan.Status.Nodes {
		if plan.Status.Nodes[i].Phase.InFlight() {
			return &plan.Status.Nodes[i]
		}
	}

	return nil
}

func failedTarget(plan *upgradev1alpha1.UpgradePlan) (string, string) {
	for i := range plan.Status.Nodes {
		if plan.Status.Nodes[i].Phase == upgradev1alpha1.NodePhaseFailed {
			return "node " + plan.Status.Nodes[i].Name, plan.Status.Nodes[i].Message
		}
	}

	if plan.Status.Kubernetes.Phase == upgradev1alpha1.NodePhaseFailed {
		return "kubernetes", plan.Status.Kubernetes.Message
	}

	return "", ""
}

func (r *UpgradePlanReconciler) handleUnfreeze(ctx context.Context, plan *upgradev1alpha1.UpgradePlan) (bool, error) {
	log := log.FromContext(ctx)

	target, ok := plan.Annotations[upgradev1alpha1.UnfreezeAnnotation]
	if !ok {
		return false, nil
	}

	reset := 0

	for i := range plan.Status.Nodes {
		node := &plan.Status.Nodes[i]
		if node.Phase != upgradev1alpha1.NodePhaseFailed {
			continue
		}

		if target == "all" || target == node.Name {
			transition(node, upgradev1alpha1.NodePhasePending, "failure acknowledged by operator")
			node.RollbackIssued = false
			node.SelfEvictionRequested = false
			node.Resends = 0
			reset++
		}
	}

	if plan.Status.Kubernetes.Phase == upgradev1alpha1.NodePhaseFailed &&
		(target == "all" || target == "kubernetes") {
		transitionKubernetes(&plan.Status.Kubernetes, upgradev1alpha1.NodePhasePending, "failure acknowledged by operator")

		reset++
	}

	log.Info("unfreeze requested", "target", target, "phasesReset", reset)

	if reset > 0 {
		r.event(plan, corev1.EventTypeNormal, "PlanUnfrozen",
			fmt.Sprintf("operator acknowledged failure of %q, %d phase(s) reset", target, reset))
	} else {
		r.event(plan, corev1.EventTypeWarning, "UnfreezeNoMatch",
			fmt.Sprintf("unfreeze %q matched no Failed target; plan stays frozen "+
				"(valid values: a node name, %q, or %q)", target, "kubernetes", "all"))
	}

	// status first: if removing the annotation fails, the retry is a no-op
	if reset > 0 {
		if err := persistStatus(ctx, r.Client, plan); err != nil {
			return false, fmt.Errorf("update status for unfreeze: %w", err)
		}
	}

	if err := removeAnnotation(ctx, r.Client, plan, upgradev1alpha1.UnfreezeAnnotation); err != nil {
		return false, err
	}

	return true, nil
}

func (r *UpgradePlanReconciler) setPhase(plan *upgradev1alpha1.UpgradePlan, phase upgradev1alpha1.PlanPhase, reason, message string) {
	frozen := phase == upgradev1alpha1.PlanPhaseFrozen
	if frozen {
		metricPlanFrozen.Set(1)

		if plan.Status.Phase != upgradev1alpha1.PlanPhaseFrozen {
			r.event(plan, corev1.EventTypeWarning, "PlanFrozen", message)
		}
	} else {
		metricPlanFrozen.Set(0)
	}

	metricPlanBlocked.Reset()

	if blockedReasons[reason] {
		metricPlanBlocked.WithLabelValues(reason).Set(1)

		if plan.Status.Reason != reason {
			r.event(plan, corev1.EventTypeWarning, "PlanBlocked", message)
		}
	}

	plan.Status.Phase = phase
	plan.Status.Reason = reason
	plan.Status.Message = message
}

func nextTalosNode(plan *upgradev1alpha1.UpgradePlan) *upgradev1alpha1.NodeStatus {
	if plan.Spec.TalosVersion == "" {
		return nil
	}

	if !talosTargetSupported(plan.Spec.TalosVersion) {
		return nil
	}

	var next *upgradev1alpha1.NodeStatus

	for i := range plan.Status.Nodes {
		n := &plan.Status.Nodes[i]
		if n.Phase != upgradev1alpha1.NodePhasePending || n.CurrentVersion == "" || n.Held {
			continue
		}

		if semver.Compare(n.CurrentVersion, plan.Spec.TalosVersion) >= 0 {
			continue
		}

		if next == nil || nodeBefore(n, next) {
			next = n
		}
	}

	return next
}

// heldNodes names the held nodes, only those below the version if one is given.
func heldNodes(plan *upgradev1alpha1.UpgradePlan, below string) []string {
	var names []string

	for _, n := range plan.Status.Nodes {
		if n.Held && (below == "" || semver.Compare(n.CurrentVersion, below) < 0) {
			names = append(names, n.Name)
		}
	}

	return names
}

func heldMessage(names []string) string {
	return fmt.Sprintf("node(s) %v held by the %s annotation", names, upgradev1alpha1.HoldAnnotation)
}

func nodeBefore(a, b *upgradev1alpha1.NodeStatus) bool {
	if a.Role != b.Role {
		return a.Role == upgradev1alpha1.RoleControlPlane
	}

	if c := semver.Compare(a.CurrentVersion, b.CurrentVersion); c != 0 {
		return c < 0
	}

	return a.Name < b.Name
}

func isNodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

func timeouts(plan *upgradev1alpha1.UpgradePlan) upgradev1alpha1.UpgradeTimeouts {
	if plan.Spec.Timeouts == nil {
		return upgradev1alpha1.UpgradeTimeouts{}
	}

	return *plan.Spec.Timeouts
}

func upgradeTimeout(p *upgradev1alpha1.UpgradePlan) time.Duration {
	return specDuration(timeouts(p).UpgradeMinutes, time.Minute, defaultUpgradeTimeout)
}

func verifyTimeout(p *upgradev1alpha1.UpgradePlan) time.Duration {
	return specDuration(timeouts(p).VerifyMinutes, time.Minute, defaultVerifyTimeout)
}

func healthCheckTimeout(p *upgradev1alpha1.UpgradePlan) time.Duration {
	return specDuration(timeouts(p).HealthCheckMinutes, time.Minute, defaultHealthCheckTimeout)
}

func rollbackTimeout(p *upgradev1alpha1.UpgradePlan) time.Duration {
	return specDuration(timeouts(p).RollbackMinutes, time.Minute, defaultRollbackTimeout)
}

// cooldownRemaining counts from the last success: a new target resets node phases.
func cooldownRemaining(now time.Time, plan *upgradev1alpha1.UpgradePlan) time.Duration {
	cooldown := specDuration(plan.Spec.CooldownMinutes, time.Minute, defaultCooldown)
	if cooldown <= 0 || plan.Status.LastCompletedAt == nil {
		return 0
	}

	return max(cooldown-now.Sub(plan.Status.LastCompletedAt.Time), 0)
}

func (r *UpgradePlanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	registerUpgradeMetrics()

	return ctrl.NewControllerManagedBy(mgr).
		For(&upgradev1alpha1.UpgradePlan{}).
		Named("upgradeplan").
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func unknownVersionNode(plan *upgradev1alpha1.UpgradePlan) string {
	for _, n := range plan.Status.Nodes {
		if n.CurrentVersion == "" {
			return n.Name
		}
	}

	return ""
}
