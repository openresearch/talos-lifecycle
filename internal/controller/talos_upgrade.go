package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
	"github.com/openresearch/talos-lifecycle/internal/talosutil"
)

// If this controller runs on the node, it evicts itself first and the new pod sends the upgrade.
func (r *UpgradePlanReconciler) startNodeUpgrade(
	ctx context.Context,
	plan *upgradev1alpha1.UpgradePlan,
	node *upgradev1alpha1.NodeStatus,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// evict once: a new pod that lands here again proceeds
	if !node.SelfEvictionRequested {
		pod, err := r.ownPodOn(ctx, node.Name)
		if err == nil && pod != nil {
			// marker first: written after the eviction, it could be lost with the pod
			previous := node.Message
			node.SelfEvictionRequested = true
			node.Message = "controller self-eviction requested"

			if err := persistStatus(ctx, r.Client, plan); err != nil {
				node.SelfEvictionRequested, node.Message = false, previous

				return ctrl.Result{}, fmt.Errorf("persist self-eviction intent for node %s: %w", node.Name, err)
			}

			if err = r.SubResource("eviction").Create(ctx, pod,
				&policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}}); err == nil {
				log.Info("controller pod runs on the upgrade target node, evicted self first", "node", node.Name)

				return ctrl.Result{RequeueAfter: pollInterval}, nil
			}

			node.SelfEvictionRequested, node.Message = false, previous
			err = fmt.Errorf("evict pod %s: %w", pod.Name, err)
		}

		if err != nil {
			r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonWaitingForEviction,
				fmt.Sprintf("controller pod runs on %s and could not be evicted: %v", node.Name, err))
			log.Info("self-eviction blocked, retrying", "node", node.Name, "error", err.Error())

			return ctrl.Result{RequeueAfter: clusterCheckInterval}, nil
		}
	}

	image, err := r.resolveInstallerImage(ctx, plan, node)
	if err != nil {
		return ctrl.Result{}, err
	}

	if yield, err := r.yieldToConfig(ctx, plan); err != nil {
		return ctrl.Result{}, err
	} else if yield {
		return ctrl.Result{RequeueAfter: clusterCheckInterval}, nil
	}

	// persisted first, so after a restart the node is polled, not sent a second upgrade
	transition(node, upgradev1alpha1.NodePhaseUpgrading, "upgrade starting")
	node.StartedAt = metav1.Now()
	node.RollbackIssued = false
	node.TargetVersion = plan.Spec.TalosVersion

	if err := persistStatus(ctx, r.Client, plan); err != nil {
		transition(node, upgradev1alpha1.NodePhasePending, "")

		return ctrl.Result{}, fmt.Errorf("persist upgrade intent for node %s: %w", node.Name, err)
	}

	if err := r.nodes().Upgrade(ctx, node.Address, image); err != nil {
		// cut off by shutdown, the call may still have reached the node: keep Upgrading
		if ctx.Err() != nil {
			return ctrl.Result{}, fmt.Errorf("upgrade call for node %s interrupted, keeping Upgrading: %w", node.Name, err)
		}

		// refused, e.g. a missing image; the node is untouched. Written onto a fresh
		// read: the call can block for minutes.
		name := node.Name
		transition(node, upgradev1alpha1.NodePhasePending, msgUpgradeRefused+err.Error())
		refused := *node
		why, retry := refusedUpgrade(&refused, refused.TargetVersion, time.Now())
		r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonUpgradeRefused, why)

		if updateErr := r.persistOnFresh(ctx, plan, func(fresh *upgradev1alpha1.UpgradePlan) {
			for i := range fresh.Status.Nodes {
				if fresh.Status.Nodes[i].Name == name {
					fresh.Status.Nodes[i] = refused
				}
			}
			fresh.Status.Phase, fresh.Status.Reason, fresh.Status.Message = upgradev1alpha1.PlanPhaseWaiting, reasonUpgradeRefused, why
		}); updateErr != nil {
			log.Error(updateErr, "persist the refused upgrade failed; node will resolve via upgrade timeout", "node", name)
		}

		log.Info("upgrade refused", "node", name, "error", err.Error())

		return ctrl.Result{RequeueAfter: retry}, nil
	}

	node.Message = "upgrade started"
	log.Info("talos upgrade started", "node", node.Name, "target", node.TargetVersion, "image", image)
	r.event(plan, corev1.EventTypeNormal, "NodeUpgradeStarted",
		fmt.Sprintf("upgrading node %s from %s to %s (image %s)",
			node.Name, node.CurrentVersion, node.TargetVersion, image))

	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

func (r *UpgradePlanReconciler) driveNodeUpgrade(
	ctx context.Context,
	plan *upgradev1alpha1.UpgradePlan,
	node *upgradev1alpha1.NodeStatus,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	target := nodeTarget(plan, node)

	switch node.Phase {
	case upgradev1alpha1.NodePhaseUpgrading:
		r.driveUpgrading(ctx, plan, node)

		return ctrl.Result{RequeueAfter: pollInterval}, nil

	case upgradev1alpha1.NodePhaseVerifying:
		// ask the node before judging the budget: it bounds the node, not a controller outage
		var k8sNode corev1.Node
		err := r.Get(ctx, client.ObjectKey{Name: node.Name}, &k8sNode)

		switch {
		case err == nil && isNodeReady(&k8sNode):
			transition(node, upgradev1alpha1.NodePhaseHealthChecking, "node ready, verifying cluster health")
		case time.Since(node.LastTransition.Time) > verifyTimeout(plan):
			r.failNode(plan, node,
				fmt.Sprintf("node did not become ready within %s after reaching %s", verifyTimeout(plan), target))
		case err != nil:
			log.V(1).Info("waiting for k8s node", "node", node.Name, "error", err.Error())
		}

		return ctrl.Result{RequeueAfter: pollInterval}, nil

	case upgradev1alpha1.NodePhaseHealthChecking:
		healthy, reason, err := r.nodes().Health(ctx, r.Client)

		// a Ready control-plane node may not be an etcd voter yet
		if err == nil && healthy && node.Role == upgradev1alpha1.RoleControlPlane {
			var quorum gateResult
			if quorum, err = r.etcdQuorumReady(ctx, plan); err == nil && !quorum.passed() {
				healthy, reason = false, "waiting for etcd quorum: "+quorum.reason
			}
		}

		switch {
		case err == nil && healthy:
		case time.Since(node.LastTransition.Time) > healthCheckTimeout(plan):
			r.failNode(plan, node,
				fmt.Sprintf("cluster health did not recover within %s after upgrading to %s",
					healthCheckTimeout(plan), target))

			return ctrl.Result{RequeueAfter: pollInterval}, nil
		case err != nil:
			return ctrl.Result{}, err
		default:
			log.V(1).Info("cluster not healthy yet", "node", node.Name, "reason", reason)
			node.Message = reason

			return ctrl.Result{RequeueAfter: pollInterval}, nil
		}

		transition(node, upgradev1alpha1.NodePhaseSucceeded, "upgraded, ready, cluster healthy")

		plan.Status.LastCompletedAt = ptrTime(time.Now())
		node.SelfEvictionRequested = false
		node.Resends = 0
		observeUpgradeResult("talos", "success", node.StartedAt.Time)
		r.event(plan, corev1.EventTypeNormal, "NodeUpgradeSucceeded",
			fmt.Sprintf("node %s upgraded to %s", node.Name, target))
		log.Info("node upgraded", "node", node.Name, "version", target)

		return ctrl.Result{RequeueAfter: pollInterval}, nil

	case upgradev1alpha1.NodePhaseRollingBack:
		return r.driveRollback(ctx, plan, node)

	default:
		return ctrl.Result{}, fmt.Errorf("unexpected phase %q while driving upgrade of node %s", node.Phase, node.Name)
	}
}

func (r *UpgradePlanReconciler) driveUpgrading(ctx context.Context, plan *upgradev1alpha1.UpgradePlan, node *upgradev1alpha1.NodeStatus) {
	target := nodeTarget(plan, node)

	var stage runtime.MachineStage

	version, err := r.nodes().Version(ctx, node.Address)
	if err == nil {
		node.CurrentVersion = version
		if version == target {
			transition(node, upgradev1alpha1.NodePhaseVerifying, "target version reached, waiting for node to become ready")

			return
		}

		var stageErr error
		if stage, stageErr = r.nodes().Stage(ctx, node.Address); stageErr != nil {
			log.FromContext(ctx).V(1).Info("could not read machine stage", "node", node.Name, "error", stageErr.Error())
		} else if upgradeNeverStarted(stage, time.Since(node.LastTransition.Time)) && node.Resends < maxUpgradeResends {
			node.Resends++
			msg := fmt.Sprintf("node %s still runs %s and is not upgrading; sending the upgrade to %s again",
				node.Name, version, target)
			transition(node, upgradev1alpha1.NodePhasePending, msg)
			r.event(plan, corev1.EventTypeWarning, "UpgradeResent", msg)

			return
		}
	}

	deadline := node.LastTransition.Add(upgradeTimeout(plan))

	switch {
	case time.Now().After(deadline):
		r.failNode(plan, node, fmt.Sprintf("node did not reach %s within %s", target, upgradeTimeout(plan)))
	case err != nil:
		log.FromContext(ctx).V(1).Info("waiting for the target version", "node", node.Name, "target", target, "error", err.Error())
	default:
		node.Message = fmt.Sprintf("waiting for %s until %s UTC; node runs %s, stage %s", target,
			deadline.UTC().Format(time.TimeOnly), version, stage)
	}
}

func (r *UpgradePlanReconciler) failNode(plan *upgradev1alpha1.UpgradePlan, node *upgradev1alpha1.NodeStatus, message string) {
	if node.Phase != upgradev1alpha1.NodePhaseRollingBack {
		observeUpgradeResult("talos", "failure", node.StartedAt.Time)
		r.event(plan, corev1.EventTypeWarning, "NodeUpgradeFailed",
			fmt.Sprintf("node %s: %s", node.Name, message))
	}

	if autoRollbackEnabled(plan) && node.Phase != upgradev1alpha1.NodePhaseRollingBack && !node.RollbackIssued {
		transition(node, upgradev1alpha1.NodePhaseRollingBack, "upgrade failed, rollback pending: "+message)
		r.event(plan, corev1.EventTypeNormal, "RollbackStarted",
			fmt.Sprintf("node %s: rolling back to previous Talos image", node.Name))

		return
	}

	transition(node, upgradev1alpha1.NodePhaseFailed, message)
}

func autoRollbackEnabled(plan *upgradev1alpha1.UpgradePlan) bool {
	return plan.Spec.AutoRollback == nil || *plan.Spec.AutoRollback
}

func isUnavailable(err error) bool {
	s, ok := status.FromError(err)

	return ok && s.Code() == codes.Unavailable
}

// RollbackIssued is persisted first: a second rollback would boot the failed image again.
func (r *UpgradePlanReconciler) driveRollback(
	ctx context.Context,
	plan *upgradev1alpha1.UpgradePlan,
	node *upgradev1alpha1.NodeStatus,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	target := nodeTarget(plan, node)

	timedOut := func() ctrl.Result {
		r.event(plan, corev1.EventTypeWarning, "RollbackFailed",
			fmt.Sprintf("node %s did not come back within %s", node.Name, rollbackTimeout(plan)))
		metricRollbacks.WithLabelValues("failure").Inc()
		r.failNode(plan, node, fmt.Sprintf("rollback did not complete within %s", rollbackTimeout(plan)))

		return ctrl.Result{RequeueAfter: pollInterval}
	}
	expired := time.Since(node.LastTransition.Time) > rollbackTimeout(plan)

	if !node.RollbackIssued {
		if expired {
			return timedOut(), nil
		}

		// on a node that never booted the target, the previous image is older: skip
		version, err := r.nodes().Version(ctx, node.Address)

		switch {
		case err == nil && version != target:
			r.event(plan, corev1.EventTypeWarning, "RollbackSkipped",
				fmt.Sprintf("node %s still runs %s and never booted %s; nothing to roll back",
					node.Name, version, target))

			node.CurrentVersion = version
			r.failNode(plan, node, fmt.Sprintf(
				"upgrade to %s failed before the node booted it; rollback skipped (node still on %s)",
				target, version))

			return ctrl.Result{RequeueAfter: pollInterval}, nil

		case err != nil && !isUnavailable(err):
			// not proven down; the problem may be on our side
			log.V(1).Info("cannot verify node state before rollback, retrying",
				"node", node.Name, "error", err.Error())

			node.Message = fmt.Sprintf("rollback pending: cannot verify node state: %v", err)

			return ctrl.Result{RequeueAfter: pollInterval}, nil
		}

		node.RollbackIssued = true
		node.Message = "issuing rollback"

		if err := persistStatus(ctx, r.Client, plan); err != nil {
			node.RollbackIssued = false

			return ctrl.Result{}, fmt.Errorf("persist rollback intent: %w", err)
		}

		if err := r.nodes().Rollback(ctx, node.Address); err != nil {
			metricRollbacks.WithLabelValues("failure").Inc()
			r.event(plan, corev1.EventTypeWarning, "RollbackFailed",
				fmt.Sprintf("node %s: rollback call failed: %v", node.Name, err))
			r.failNode(plan, node, fmt.Sprintf("rollback call failed: %v", err))

			return ctrl.Result{RequeueAfter: pollInterval}, nil
		}

		node.Message = "rollback issued, waiting for node to boot the previous image"
		log.Info("rollback issued", "node", node.Name)

		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}

	version, err := r.nodes().Version(ctx, node.Address)
	if err == nil && version != target {
		node.CurrentVersion = version
	}

	var k8sNode corev1.Node
	back := err == nil && version != target &&
		r.Get(ctx, client.ObjectKey{Name: node.Name}, &k8sNode) == nil && isNodeReady(&k8sNode)

	switch {
	case back:
	case expired:
		return timedOut(), nil
	case err != nil:
		log.V(1).Info("waiting for node during rollback", "node", node.Name, "error", err.Error())

		return ctrl.Result{RequeueAfter: pollInterval}, nil
	default:
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}

	metricRollbacks.WithLabelValues("success").Inc()
	r.event(plan, corev1.EventTypeNormal, "RollbackSucceeded",
		fmt.Sprintf("node %s rolled back to %s and is Ready", node.Name, version))
	r.failNode(plan, node,
		fmt.Sprintf("upgrade to %s failed; node rolled back to %s and is Ready; investigate, then unfreeze",
			target, version))

	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

// ownPodOn finds the pod by POD_NAME: a label would also match the config controller.
func (r *UpgradePlanReconciler) ownPodOn(ctx context.Context, nodeName string) (*corev1.Pod, error) {
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		return nil, nil
	}

	namespace, err := talosutil.Namespace()
	if err != nil {
		return nil, fmt.Errorf("get namespace: %w", err)
	}

	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: podName}, &pod); err != nil {
		return nil, fmt.Errorf("get own pod %s: %w", podName, err)
	}

	if pod.Spec.NodeName != nodeName {
		return nil, nil
	}

	return &pod, nil
}

func nodeTarget(plan *upgradev1alpha1.UpgradePlan, node *upgradev1alpha1.NodeStatus) string {
	if node.TargetVersion != "" {
		return node.TargetVersion
	}

	return plan.Spec.TalosVersion
}

const msgUpgradeRefused = "upgrade call refused: "

func refusedUpgrade(n *upgradev1alpha1.NodeStatus, target string, now time.Time) (string, time.Duration) {
	why, ok := strings.CutPrefix(n.Message, msgUpgradeRefused)
	retry := n.LastTransition.Add(upgradeRefusedRetry)

	if !ok || n.Phase != upgradev1alpha1.NodePhasePending || n.TargetVersion != target || !now.Before(retry) {
		return "", 0
	}

	return fmt.Sprintf("node %s refused the upgrade to %s, the node is untouched: %s; retrying at %s UTC, "+
		"or fix the target or spec.installerImageBase", n.Name, target, why, retry.UTC().Format(time.TimeOnly)), retry.Sub(now)
}

// Talos enters the upgrading stage right after the Upgrade call returns.
const upgradeStartGrace = 2 * time.Minute

// An image that fails to boot and falls back looks the same: do not loop.
const maxUpgradeResends = 1

func upgradeNeverStarted(stage runtime.MachineStage, sinceCall time.Duration) bool {
	return stage == runtime.MachineStageRunning && sinceCall > upgradeStartGrace
}
