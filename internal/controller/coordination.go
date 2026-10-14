package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

// The two loops yield: neither starts on a node while the other has one in
// flight. Not a lock; a config try that slips through reverts on its own.

func upgradeInFlight(ctx context.Context, r client.Reader) (string, error) {
	var plan upgradev1alpha1.UpgradePlan
	if err := r.Get(ctx, client.ObjectKey{Name: upgradev1alpha1.UpgradePlanName}, &plan); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return "", nil
		}

		return "", fmt.Errorf("read UpgradePlan: %w", err)
	}

	for _, n := range plan.Status.Nodes {
		if n.Phase.InFlight() {
			return "node " + n.Name, nil
		}
	}

	if plan.Status.Phase == upgradev1alpha1.PlanPhaseUpgrading {
		return "the Kubernetes upgrade", nil
	}

	return "", nil
}

func configInFlight(ctx context.Context, r client.Reader) (string, error) {
	var plan upgradev1alpha1.MachineConfigPlan
	if err := r.Get(ctx, client.ObjectKey{Name: upgradev1alpha1.MachineConfigPlanName}, &plan); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return "", nil
		}

		return "", fmt.Errorf("read MachineConfigPlan: %w", err)
	}

	for _, n := range plan.Status.Nodes {
		if n.Phase.InFlight() {
			return n.Name, nil
		}
	}

	return "", nil
}
