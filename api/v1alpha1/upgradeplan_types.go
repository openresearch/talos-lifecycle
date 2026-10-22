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

package v1alpha1

import (
	"golang.org/x/mod/semver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const UpgradePlanName = "cluster"

const (
	RoleControlPlane = "control-plane"
	RoleWorker       = "worker"
)

const (
	// EtcdSnapshotRequired waits for a stored snapshot before control-plane and Kubernetes steps.
	EtcdSnapshotRequired = "required"
	EtcdSnapshotDisabled = "disabled"
)

const (
	ChannelManual = "manual"
	// ChannelLatest: the controller advances the versions one stable release at a time.
	ChannelLatest = "latest"
)

type NodePhase string

const (
	NodePhasePending NodePhase = "Pending"
	// NodePhaseUpgrading: sent; the node installs and reboots.
	NodePhaseUpgrading NodePhase = "Upgrading"
	// NodePhaseVerifying: at the target version, not yet Ready.
	NodePhaseVerifying NodePhase = "Verifying"
	// NodePhaseHealthChecking: Ready; waiting for the cluster health check.
	NodePhaseHealthChecking NodePhase = "HealthChecking"
	NodePhaseSucceeded      NodePhase = "Succeeded"
	// NodePhaseRollingBack always ends in Failed.
	NodePhaseRollingBack NodePhase = "RollingBack"
	// NodePhaseFailed freezes the plan until unfrozen.
	NodePhaseFailed NodePhase = "Failed"
)

func (p NodePhase) InFlight() bool {
	switch p {
	case NodePhaseUpgrading, NodePhaseVerifying, NodePhaseHealthChecking, NodePhaseRollingBack:
		return true
	}

	return false
}

type PlanPhase string

const (
	PlanPhaseIdle      PlanPhase = "Idle"
	PlanPhaseUpgrading PlanPhase = "Upgrading"
	// PlanPhaseWaiting: a gate that clears without an operator; status.reason names it.
	PlanPhaseWaiting PlanPhase = "Waiting"
	PlanPhaseFrozen  PlanPhase = "Frozen"
)

// UnfreezeAnnotation resets Failed entries to Pending: a node name, "all", or on
// the UpgradePlan "kubernetes". The controller removes it afterwards.
const UnfreezeAnnotation = "talos.openresearch.com/unfreeze"

// UpgradeWindow: "HH:MM" in the controller's timezone; Start after End wraps
// around midnight. A step may finish outside the window.
type UpgradeWindow struct {
	// +kubebuilder:validation:Pattern=`^([01]\d|2[0-3]):[0-5]\d$`
	Start string `json:"start"`
	// +kubebuilder:validation:Pattern=`^([01]\d|2[0-3]):[0-5]\d$`
	End string `json:"end"`
}

type UpgradePlanSpec struct {
	// "manual": an operator sets the versions, and setting one approves the upgrade.
	// "latest": the controller sets them, one stable release at a time.
	// +kubebuilder:validation:Enum=manual;latest
	// +kubebuilder:default=manual
	// +optional
	Channel string `json:"channel,omitempty"`

	// Minimum Talos version for every node, e.g. "v1.10.2". Nodes already above
	// it are left alone, never downgraded. Empty leaves Talos alone.
	// +kubebuilder:validation:Pattern=`^v\d+\.\d+\.\d+$`
	// +optional
	TalosVersion string `json:"talosVersion,omitempty"`

	// Kubernetes version, e.g. "v1.32.3". Empty leaves Kubernetes alone.
	// +kubebuilder:validation:Pattern=`^v\d+\.\d+\.\d+$`
	// +optional
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`

	// Pause after each successful step. 0 disables it.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=0
	// +optional
	CooldownMinutes *int64 `json:"cooldownMinutes,omitempty"`

	// Per-phase timeouts. Too short fails, and may roll back, a healthy upgrade.
	// +optional
	Timeouts *UpgradeTimeouts `json:"timeouts,omitempty"`

	// When a step may start. Unset: any time.
	// +optional
	Window *UpgradeWindow `json:"window,omitempty"`

	// Installer image without tag, for every node. Empty: the node's own Image
	// Factory schematic, else the controller's default.
	// +optional
	InstallerImageBase string `json:"installerImageBase,omitempty"`

	// "required": wait for an etcd snapshot in S3 before every control-plane and
	// Kubernetes step. "disabled": skip it.
	// +kubebuilder:validation:Enum=required;disabled
	// +kubebuilder:default=required
	// +optional
	EtcdSnapshotPolicy string `json:"etcdSnapshotPolicy,omitempty"`

	// Boot a node's previous Talos image when its upgrade fails. The plan freezes
	// either way. Kubernetes is never rolled back.
	// +kubebuilder:default=true
	// +optional
	AutoRollback *bool `json:"autoRollback,omitempty"`
}

// UpgradeTimeouts are in minutes.
type UpgradeTimeouts struct {
	// Install and reboot of a node (default 30).
	// +kubebuilder:validation:Minimum=1
	// +optional
	UpgradeMinutes *int64 `json:"upgradeMinutes,omitempty"`
	// Rebooted node to Ready (default 15).
	// +kubebuilder:validation:Minimum=1
	// +optional
	VerifyMinutes *int64 `json:"verifyMinutes,omitempty"`
	// Cluster health after an upgrade (default 15).
	// +kubebuilder:validation:Minimum=1
	// +optional
	HealthCheckMinutes *int64 `json:"healthCheckMinutes,omitempty"`
	// Rolled-back node to Ready (default 20).
	// +kubebuilder:validation:Minimum=1
	// +optional
	RollbackMinutes *int64 `json:"rollbackMinutes,omitempty"`
}

type EtcdSnapshotStatus struct {
	// Object key in the bucket.
	Name string      `json:"name"`
	Time metav1.Time `json:"time"`
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

type NodeStatus struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	// +kubebuilder:validation:Enum=control-plane;worker
	Role string `json:"role"`
	// Talos version the node last reported.
	// +optional
	CurrentVersion string `json:"currentVersion,omitempty"`
	// +optional
	Phase NodePhase `json:"phase,omitempty"`
	// +optional
	LastTransition metav1.Time `json:"lastTransition,omitempty"`
	// +optional
	StartedAt metav1.Time `json:"startedAt,omitempty"`
	// Set before the rollback is sent, so a restart never sends a second one.
	// +optional
	RollbackIssued bool `json:"rollbackIssued,omitempty"`
	// Upgrades sent again because the node never started the previous one.
	// +optional
	Resends int32 `json:"resends,omitempty"`
	// Version the node was sent; it is judged against this, not the current spec.
	// +optional
	TargetVersion string `json:"targetVersion,omitempty"`
	// The controller evicted its own pod from this node; a new pod landing here
	// proceeds instead of evicting again.
	// +optional
	SelfEvictionRequested bool `json:"selfEvictionRequested,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

type KubernetesStatus struct {
	// Lowest kubelet version in the cluster.
	// +optional
	CurrentVersion string `json:"currentVersion,omitempty"`
	// +optional
	Phase NodePhase `json:"phase,omitempty"`
	// +optional
	LastTransition metav1.Time `json:"lastTransition,omitempty"`
	// +optional
	StartedAt metav1.Time `json:"startedAt,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`

	// +optional
	TargetVersion string `json:"targetVersion,omitempty"`
}

type UpgradePlanStatus struct {
	// Stable Talos releases newer than the lowest node version.
	// +optional
	AvailableTalosVersions []string `json:"availableTalosVersions,omitempty"`

	// Stable Kubernetes releases newer than the cluster's.
	// +optional
	AvailableKubernetesVersions []string `json:"availableKubernetesVersions,omitempty"`

	// +optional
	Nodes []NodeStatus `json:"nodes,omitempty"`

	// +optional
	Kubernetes KubernetesStatus `json:"kubernetes,omitempty"`

	// +optional
	LastEtcdSnapshot *EtcdSnapshotStatus `json:"lastEtcdSnapshot,omitempty"`

	// Last successful node or Kubernetes step; the cooldown counts from here.
	// +optional
	LastCompletedAt *metav1.Time `json:"lastCompletedAt,omitempty"`

	// +optional
	Phase PlanPhase `json:"phase,omitempty"`

	// Machine-readable cause, e.g. "CoolingDown".
	// +optional
	Reason string `json:"reason,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
}

func (s UpgradePlanStatus) LowestTalosVersion() string {
	lowest := ""

	for _, n := range s.Nodes {
		if n.CurrentVersion == "" {
			continue
		}

		if lowest == "" || semver.Compare(n.CurrentVersion, lowest) < 0 {
			lowest = n.CurrentVersion
		}
	}

	return lowest
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`
// +kubebuilder:printcolumn:name="Talos",type=string,JSONPath=`.spec.talosVersion`
// +kubebuilder:printcolumn:name="Kubernetes",type=string,JSONPath=`.spec.kubernetesVersion`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// UpgradePlan is the cluster-scoped singleton "cluster": desired Talos and
// Kubernetes versions, and upgrade progress.
type UpgradePlan struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UpgradePlanSpec   `json:"spec,omitempty"`
	Status UpgradePlanStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type UpgradePlanList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UpgradePlan `json:"items"`
}

func init() {
	SchemeBuilder.Register(&UpgradePlan{}, &UpgradePlanList{})
}
