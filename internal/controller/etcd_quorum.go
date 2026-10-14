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

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
	"github.com/openresearch/talos-lifecycle/internal/talosutil"
)

// learners do not vote
func (r *UpgradePlanReconciler) etcdQuorumReady(
	ctx context.Context,
	plan *upgradev1alpha1.UpgradePlan,
) (gateResult, error) {
	controlPlanes := controlPlaneAddresses(plan)

	lastErr := errors.New("no control-plane node has a known address")

	for _, address := range controlPlanes {
		voters, learners, err := etcdVoterCount(ctx, address)
		if err != nil {
			lastErr = err

			continue
		}

		if voters < 3 {
			reason := fmt.Sprintf(
				"etcd has %d voting member(s); 3 are needed to upgrade a control-plane node without losing quorum",
				voters)
			if learners > 0 {
				reason += fmt.Sprintf(" (%d member(s) still joining as learners)", learners)
			}

			return gateResult{reason: reason}, nil
		}

		return gateResult{}, nil
	}

	return blocked("etcd membership could not be read from any control-plane node: %v", lastErr), nil
}

func etcdVoterCount(ctx context.Context, address string) (voters, learners int, err error) {
	c, talosCtx, err := talosutil.Client(ctx, address)
	if err != nil {
		return 0, 0, err
	}
	defer c.Close() //nolint:errcheck

	resp, err := c.EtcdMemberList(talosCtx, &machineapi.EtcdMemberListRequest{})
	if err != nil {
		return 0, 0, fmt.Errorf("list etcd members via %s: %w", address, err)
	}

	for _, message := range resp.GetMessages() {
		for _, member := range message.GetMembers() {
			if member.GetIsLearner() {
				learners++

				continue
			}

			voters++
		}
	}

	return voters, learners, nil
}
