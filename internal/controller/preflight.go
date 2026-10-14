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

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

var apiServerSelector = client.MatchingLabels{"k8s-app": "kube-apiserver"}

func (r *UpgradePlanReconciler) controlPlaneReady(
	ctx context.Context,
	plan *upgradev1alpha1.UpgradePlan,
) (gateResult, error) {
	if result, err := r.controlPlaneHealthy(ctx, plan); err != nil || !result.passed() {
		return result, err
	}

	return r.etcdQuorumReady(ctx, plan)
}

func (r *UpgradePlanReconciler) nodesReady(ctx context.Context) (gateResult, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return gateResult{}, fmt.Errorf("list nodes: %w", err)
	}

	for i := range nodes.Items {
		n := &nodes.Items[i]
		if !isNodeReady(n) {
			return blocked("node %s is not ready", n.Name), nil
		}

		if n.Spec.Unschedulable {
			return blocked("node %s is unschedulable", n.Name), nil
		}
	}

	return gateResult{}, nil
}

// controlPlaneHealthy checks for a Ready kube-apiserver on every control-plane
// node: node Ready only means the kubelet runs.
func (r *UpgradePlanReconciler) controlPlaneHealthy(
	ctx context.Context,
	plan *upgradev1alpha1.UpgradePlan,
) (gateResult, error) {
	expected := controlPlaneNames(plan)
	if len(expected) == 0 {
		return gateResult{}, nil
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, apiServerSelector); err != nil {
		return gateResult{}, fmt.Errorf("list kube-apiserver pods: %w", err)
	}

	serving := make(map[string]bool, len(expected))

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != "" && isPodReady(pod) {
			serving[pod.Spec.NodeName] = true
		}
	}

	var missing []string

	for _, name := range expected {
		if !serving[name] {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		return blocked("control-plane node(s) %v are Ready but run no kube-apiserver; the control plane is not highly available",
			missing), nil
	}

	return gateResult{}, nil
}

func controlPlaneNames(plan *upgradev1alpha1.UpgradePlan) []string {
	var names []string

	for i := range plan.Status.Nodes {
		if plan.Status.Nodes[i].Role == upgradev1alpha1.RoleControlPlane {
			names = append(names, plan.Status.Nodes[i].Name)
		}
	}

	return names
}

func isPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}

	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}
