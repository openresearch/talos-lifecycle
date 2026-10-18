# Architecture

One image, two controllers. Each runs as its own Deployment of the same binary
(`--controllers=upgrade` or `--controllers=config`) with its own
ServiceAccount, RBAC and leader-election lease, so a fault in one cannot stop
the other. Each drives a cluster-scoped singleton named `cluster`:
`UpgradePlan` and `MachineConfigPlan` in `talos.openresearch.com/v1alpha1`.

## Shared rules

- **Level-based.** Every pass reads the plan's status, does at most one thing
  and writes status. A restarted controller resumes from status.
- **One node at a time.** Each loop has at most one node in flight, and each
  waits while the other loop has a node in flight.
- **Intent before action.** Every mutating call (upgrade, rollback,
  self-eviction, config apply) is recorded in status first, so a restart
  never sends it twice.
- **Gates, not errors.** A precondition the cluster or an operator can resolve
  shows as `phase: Waiting` with a readable `reason`.
- **Failures freeze.** A failed node freezes its whole plan until an operator
  acknowledges it with the `talos.openresearch.com/unfreeze` annotation.

## Upgrade controller

Upgrades Talos node by node and Kubernetes cluster-wide toward
`spec.talosVersion` and `spec.kubernetesVersion`. Setting a version is the
approval. A leader-elected poller reads the Talos and Kubernetes releases
every 24 hours and lists the newer versions in status; with
`channel: latest` the controller sets the targets itself, one minor at a time.

Node phases:

```
Pending -> Upgrading -> Verifying -> HealthChecking -> Succeeded
                  \           \               \
                   +-----------+---------------+-> RollingBack -> Failed
```

- **Order.** Control-plane nodes first, lowest version first. Talos before
  Kubernetes, unless the Talos target needs a newer Kubernetes first.
- **Before a control-plane step.** Every control-plane node runs a
  kube-apiserver, etcd has three voting members, and an etcd snapshot is
  streamed to S3 (`etcdSnapshotPolicy: required` by default).
- **After each node.** The node runs the target, is `Ready`, and the Talos
  cluster health check passes; on control-plane nodes etcd has its voters back.
- **On timeout.** The node is rolled back to its previous image through the
  Talos A/B boot partition (`autoRollback`, on by default), then marked
  `Failed`. Kubernetes is never rolled back automatically; the etcd snapshot
  is the recovery path.
- **Compatibility.** No step leaves Talos and Kubernetes on a pair Talos does
  not support. The Talos and Kubernetes upgrade paths are compiled in from the
  `siderolabs` libraries, so a new minor needs a controller release.
- **Self-eviction.** If the controller runs on the node it is about to upgrade,
  it evicts itself first; the new pod resumes from status.
- **Pacing.** `cooldownMinutes` between upgrades (default 60) and an optional
  daily `window`.

## Config controller

Applies Talos machine config from a Git repository. The tree holds Talos
patches (`base/`, `roles/<role>.yaml`, `nodes/<name>.yaml`) that are applied onto
each node's own config, so it contains only the fields it manages and no
secrets.

The design follows from two facts about Talos:

1. Talos validation accepts a config that gives a node the wrong IP or an
   unreachable gateway.
2. Once such a config is persisted, the node boots it every time. Recovery
   needs a console.

TRY mode is the only Talos apply mode that reverts on its own: it changes the
running config, never the persisted one, and the node restores the previous
config when its timer expires or when it reboots. The controller uses it as a
two-phase commit:

```
TRY (node arms its own revert timer)
 -> probe: the node answers and runs the tried bytes
 -> soak (default 120 s)
 -> cluster health check
 -> commit: NO_REBOOT with the same bytes (persists, cancels the timer)
```

If anything fails, the controller sends nothing and waits for the node to
revert itself, confirms the revert, and freezes the plan. A stranded node
recovers without the controller, the network or the API having to work.

- **Safe by default.** `mode: DiffOnly` reports per-node diffs (secret values
  redacted, secret changes shown as hashes) and applies nothing until set to
  `Reconcile`.
- **Order.** Workers first, so a bad config stops before it reaches etcd; the
  controller's own node last within its role.
- **Reboot-only fields** (etcd settings, kernel, disk encryption, among
  others) cannot be verified by TRY. They keep the node's current value and
  are reported in status and as a warning event.
- **Git** is cloned directly over SSH with mandatory host key checking, every
  pass while a node is in flight and every 2 minutes when idle. If Git is down,
  a node in flight finishes on the last tree and nothing new starts.

## Permissions

Both controllers need the Talos `os:admin` role: `Upgrade`, `Rollback` and
`ApplyConfiguration` require it. `os:admin` can fetch an admin kubeconfig, so
the controllers' namespace is a cluster-admin boundary.

## Observability

Every transition emits a Kubernetes Event on the plan. Prometheus metrics use
the `talos_lifecycle_` prefix; alert rules for frozen, blocked, failed and
stuck plans, stale version polls and failing snapshots ship in
`config/prometheus/rules.yaml` and the chart.
