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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const MachineConfigPlanName = "cluster"

const (
	ModeReconcile = "Reconcile"
	ModeDiffOnly  = "DiffOnly"
)

type ConfigNodePhase string

const (
	// ConfigNodePending: the rendered config differs from the persistent slot.
	ConfigNodePending ConfigNodePhase = "Pending"
	// ConfigNodeTrying: TRY is about to be or was sent.
	ConfigNodeTrying ConfigNodePhase = "Trying"
	// ConfigNodeProbing: waiting for the node to run the tried config.
	ConfigNodeProbing ConfigNodePhase = "Probing"
	// ConfigNodeHealthChecking: soak, then the cluster health check.
	ConfigNodeHealthChecking ConfigNodePhase = "HealthChecking"
	// ConfigNodeCommitting: sending the tried bytes again with NO_REBOOT.
	ConfigNodeCommitting ConfigNodePhase = "Committing"
	ConfigNodeSucceeded  ConfigNodePhase = "Succeeded"
	// ConfigNodeReverted: verification failed; waiting for the node's own revert.
	ConfigNodeReverted ConfigNodePhase = "Reverted"
	// ConfigNodeFailed freezes the plan.
	ConfigNodeFailed ConfigNodePhase = "Failed"
)

// InFlight includes Reverted: the revert still has to be confirmed.
func (p ConfigNodePhase) InFlight() bool {
	switch p {
	case ConfigNodeTrying, ConfigNodeProbing, ConfigNodeHealthChecking, ConfigNodeCommitting, ConfigNodeReverted:
		return true
	}

	return false
}

type ConfigPlanPhase string

const (
	ConfigPlanIdle       ConfigPlanPhase = "Idle"
	ConfigPlanDiffing    ConfigPlanPhase = "Diffing"
	ConfigPlanRollingOut ConfigPlanPhase = "RollingOut"
	// ConfigPlanWaiting: a gate that clears without an operator.
	ConfigPlanWaiting ConfigPlanPhase = "Waiting"
	ConfigPlanFrozen  ConfigPlanPhase = "Frozen"
)

// ConfigSource holds base/, roles/ and nodes/ of Talos patches, applied onto each
// node's own config. It holds no secrets.
type ConfigSource struct {
	// +optional
	Git *GitSource `json:"git,omitempty"`

	// A local directory instead of Git; for `make run` and tests.
	// +optional
	Path string `json:"path,omitempty"`
}

type GitSource struct {
	// ssh:// or scp-style.
	URL string `json:"url"`
	// +kubebuilder:default=main
	// +optional
	Ref string `json:"ref,omitempty"`
	// Directory holding base/, roles/ and nodes/.
	// +optional
	Path string `json:"path,omitempty"`
	// Secret with `identity` (a read-only deploy key) and `known_hosts`.
	SecretRef SecretReference `json:"secretRef"`
}

type SecretReference struct {
	Name string `json:"name"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ConfigVerifyTimeouts are in seconds. tryTimeoutSeconds must exceed the others
// summed plus a minute; the controller refuses a plan that does not.
type ConfigVerifyTimeouts struct {
	// When the node reverts on its own.
	// +kubebuilder:default=600
	// +kubebuilder:validation:Minimum=60
	// +optional
	TryTimeoutSeconds *int64 `json:"tryTimeoutSeconds,omitempty"`

	// Wait for the node to run the tried config.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=10
	// +optional
	ProbeTimeoutSeconds *int64 `json:"probeTimeoutSeconds,omitempty"`

	// How long the node runs the tried config before the health check.
	// +kubebuilder:default=120
	// +kubebuilder:validation:Minimum=0
	// +optional
	SoakSeconds *int64 `json:"soakSeconds,omitempty"`

	// Cluster health check after the soak.
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=10
	// +optional
	HealthTimeoutSeconds *int64 `json:"healthTimeoutSeconds,omitempty"`
}

type MachineConfigPlanSpec struct {
	// +optional
	Source ConfigSource `json:"source,omitempty"`

	// DiffOnly (default) reports and applies nothing; Reconcile applies.
	// +kubebuilder:validation:Enum=Reconcile;DiffOnly
	// +kubebuilder:default=DiffOnly
	// +optional
	Mode string `json:"mode,omitempty"`

	// Start no new node; a node in flight finishes.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// +optional
	Verify *ConfigVerifyTimeouts `json:"verify,omitempty"`

	// Pause between nodes.
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=0
	// +optional
	CooldownMinutes *int64 `json:"cooldownMinutes,omitempty"`

	// When a node may start.
	// +optional
	Window *UpgradeWindow `json:"window,omitempty"`
}

type ConfigNodeStatus struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	// +kubebuilder:validation:Enum=control-plane;worker
	Role string `json:"role"`

	// +optional
	Phase ConfigNodePhase `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	LastTransition metav1.Time `json:"lastTransition,omitempty"`

	// Source revision this node last matched.
	// +optional
	AppliedRevision string `json:"appliedRevision,omitempty"`
	// Hash of the rendered config.
	// +optional
	DesiredHash string `json:"desiredHash,omitempty"`
	// Hash of the persistent slot.
	// +optional
	ObservedHash string `json:"observedHash,omitempty"`
	// Redacted diff while Pending; changed secrets show as hashes.
	// +optional
	Diff string `json:"diff,omitempty"`
	// +optional
	Warnings []string `json:"warnings,omitempty"`
	// Fields Talos reads only at boot; they keep the node's value, since a hot
	// apply cannot verify them.
	// +optional
	Skipped []string `json:"skipped,omitempty"`

	// Hash of what TRY sent; the commit sends the same bytes.
	// +optional
	TriedHash string `json:"triedHash,omitempty"`
	// +optional
	TryIssuedAt *metav1.Time `json:"tryIssuedAt,omitempty"`
	// When the node reverts on its own.
	// +optional
	TryDeadline *metav1.Time `json:"tryDeadline,omitempty"`
	// +optional
	RevertReason string `json:"revertReason,omitempty"`
	// Last commit; the cooldown counts from here.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

type MachineConfigPlanStatus struct {
	// +optional
	Phase ConfigPlanPhase `json:"phase,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`

	// Content hash of the tree.
	// +optional
	SourceRevision string `json:"sourceRevision,omitempty"`
	// +optional
	SourceCommit string `json:"sourceCommit,omitempty"`
	// Revision every node matches.
	// +optional
	AppliedRevision string `json:"appliedRevision,omitempty"`

	// +optional
	Nodes []ConfigNodeStatus `json:"nodes,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=mcp
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.status.appliedRevision`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MachineConfigPlan is the cluster-scoped singleton "cluster" that rolls Talos
// machine config out from Git.
type MachineConfigPlan struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MachineConfigPlanSpec   `json:"spec,omitempty"`
	Status MachineConfigPlanStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type MachineConfigPlanList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MachineConfigPlan `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MachineConfigPlan{}, &MachineConfigPlanList{})
}
