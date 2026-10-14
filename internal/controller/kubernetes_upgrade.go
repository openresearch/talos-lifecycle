package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/mod/semver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

// The rollout blocks the single reconcile worker for its duration.
func (r *UpgradePlanReconciler) startKubernetesUpgrade(
	ctx context.Context,
	plan *upgradev1alpha1.UpgradePlan,
) (ctrl.Result, error) {
	if yield, err := r.yieldToConfig(ctx, plan); err != nil {
		return ctrl.Result{}, err
	} else if yield {
		return ctrl.Result{RequeueAfter: clusterCheckInterval}, nil
	}

	transitionKubernetes(&plan.Status.Kubernetes, upgradev1alpha1.NodePhaseUpgrading, "upgrade started")
	plan.Status.Kubernetes.StartedAt = metav1.Now()
	plan.Status.Kubernetes.TargetVersion = plan.Spec.KubernetesVersion
	r.event(plan, corev1.EventTypeNormal, "KubernetesUpgradeStarted",
		fmt.Sprintf("upgrading kubernetes from %s to %s",
			plan.Status.Kubernetes.CurrentVersion, plan.Spec.KubernetesVersion))
	// persisted first, so a restart resumes the rollout
	if err := persistStatus(ctx, r.Client, plan); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status before kubernetes upgrade: %w", err)
	}

	return r.driveKubernetesUpgrade(ctx, plan)
}

func (r *UpgradePlanReconciler) driveKubernetesUpgrade(
	ctx context.Context,
	plan *upgradev1alpha1.UpgradePlan,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	target := kubernetesTarget(plan)

	endpoint, err := controlPlaneEndpoint(plan)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve control-plane endpoint: %w", err)
	}

	from, err := r.nodes().KubernetesVersion(ctx, endpoint)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("detect lowest kubernetes version: %w", err)
	}

	// a control plane at the target is not done (kubelets and manifests follow);
	// only one above the target is skipped, or the rollout would downgrade it
	done := semver.Compare(from, target) > 0
	if !done {
		err = r.nodes().UpgradeKubernetes(ctx, endpoint, from, target)
	}

	switch {
	case err != nil && ctx.Err() != nil:
		return ctrl.Result{}, fmt.Errorf("kubernetes upgrade to %s interrupted, resuming: %w", target, err)
	case errors.Is(err, errKubernetesUpgradeFailed):
		r.failKubernetes(plan, err.Error())
	case err != nil:
		return ctrl.Result{}, err
	case done:
		transitionKubernetes(&plan.Status.Kubernetes, upgradev1alpha1.NodePhaseHealthChecking,
			"control plane already above the target, verifying cluster health")
	default:
		transitionKubernetes(&plan.Status.Kubernetes, upgradev1alpha1.NodePhaseHealthChecking,
			"components upgraded, verifying cluster health")
		log.Info("kubernetes components upgraded, verifying cluster health", "version", target)
	}

	// the plan is stale after the rollout; a conflict must not cause a second one
	k := plan.Status.Kubernetes

	if err := r.persistOnFresh(ctx, plan, func(fresh *upgradev1alpha1.UpgradePlan) { fresh.Status.Kubernetes = k }); err != nil {
		return ctrl.Result{}, fmt.Errorf("persist kubernetes phase after rollout: %w", err)
	}

	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

func (r *UpgradePlanReconciler) persistOnFresh(
	ctx context.Context, plan *upgradev1alpha1.UpgradePlan, change func(*upgradev1alpha1.UpgradePlan),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh upgradev1alpha1.UpgradePlan
		if err := uncached(r.APIReader, r.Client).Get(ctx, client.ObjectKeyFromObject(plan), &fresh); err != nil {
			return err
		}

		change(&fresh)

		if err := r.Status().Update(ctx, &fresh); err != nil {
			return err
		}

		*plan = fresh

		return nil
	})
}

func (r *UpgradePlanReconciler) driveKubernetesHealthCheck(
	ctx context.Context,
	plan *upgradev1alpha1.UpgradePlan,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	target := kubernetesTarget(plan)
	k := &plan.Status.Kubernetes

	healthy, reason, err := r.nodes().Health(ctx, r.Client)

	switch {
	case err == nil && healthy:
	case time.Since(k.LastTransition.Time) > healthCheckTimeout(plan):
		r.failKubernetes(plan, fmt.Sprintf("cluster health did not recover within %s after upgrading to %s",
			healthCheckTimeout(plan), target))

		return ctrl.Result{RequeueAfter: pollInterval}, nil
	case err != nil:
		return ctrl.Result{}, err
	default:
		log.V(1).Info("cluster not healthy yet after kubernetes upgrade", "reason", reason)
		k.Message = reason

		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}

	transitionKubernetes(k, upgradev1alpha1.NodePhaseSucceeded, "upgraded, cluster healthy")

	plan.Status.LastCompletedAt = ptrTime(time.Now())
	k.CurrentVersion = target
	observeUpgradeResult("kubernetes", "success", k.StartedAt.Time)
	r.event(plan, corev1.EventTypeNormal, "KubernetesUpgradeSucceeded",
		fmt.Sprintf("kubernetes upgraded to %s", target))
	log.Info("kubernetes upgrade complete", "version", target)

	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

func (r *UpgradePlanReconciler) failKubernetes(plan *upgradev1alpha1.UpgradePlan, message string) {
	transitionKubernetes(&plan.Status.Kubernetes, upgradev1alpha1.NodePhaseFailed, message)
	observeUpgradeResult("kubernetes", "failure", plan.Status.Kubernetes.StartedAt.Time)
	r.event(plan, corev1.EventTypeWarning, "KubernetesUpgradeFailed", message)
}

func controlPlaneAddresses(plan *upgradev1alpha1.UpgradePlan) []string {
	var addresses []string

	for _, n := range plan.Status.Nodes {
		if n.Role == upgradev1alpha1.RoleControlPlane && n.Address != "" {
			addresses = append(addresses, n.Address)
		}
	}

	return addresses
}

func controlPlaneEndpoint(plan *upgradev1alpha1.UpgradePlan) (string, error) {
	controlPlanes := controlPlaneAddresses(plan)
	if len(controlPlanes) == 0 {
		return "", fmt.Errorf("no control-plane node with an address found")
	}

	return controlPlanes[0], nil
}

func kubernetesTarget(plan *upgradev1alpha1.UpgradePlan) string {
	if t := plan.Status.Kubernetes.TargetVersion; t != "" {
		return t
	}

	return plan.Spec.KubernetesVersion
}
