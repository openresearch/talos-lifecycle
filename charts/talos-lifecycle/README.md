# talos-lifecycle

Two controllers for Talos clusters, one image:

- **Upgrade controller** (`UpgradePlan`): upgrades Talos node by node and
  Kubernetes cluster-wide, with health gates, etcd quorum checks, etcd
  snapshots, automatic rollback and a freeze on failure.
- **Config controller** (`MachineConfigPlan`): rolls Talos machine config out
  from Git with try-verify-commit. Each node runs the new config on its own
  revert timer, is probed and soaked, and only then is the config persisted.

Either can be turned off (`upgrade.enabled`, `config.enabled`).

## Prerequisites

Talos must let pods in the release namespace ask for Talos credentials. Add this
document to the machine config of the control-plane nodes (Talos 1.14
machinery; it is a control-plane-only document):

```yaml
apiVersion: v1alpha1
kind: KubeTalosAPIAccessConfig
allowedRoles:
  - os:admin
allowedKubernetesNamespaces:
  - talos-lifecycle-system
```

Older Talos versions use `machine.features.kubernetesTalosAPIAccess` (`enabled:
true` plus the same two lists) instead; Talos 1.14 deprecates that field, and the
two cannot be set together.

Both controllers need `os:admin`: Talos requires it for `Upgrade`, `Rollback`
and `ApplyConfiguration`.

## Install

```sh
kubectl create namespace talos-lifecycle-system
kubectl label namespace talos-lifecycle-system pod-security.kubernetes.io/enforce=restricted
helm install talos-lifecycle oci://ghcr.io/openresearch/charts/talos-lifecycle \
  --version <version> -n talos-lifecycle-system
```

Then:

- Upgrades: set `spec.channel: latest` on the `UpgradePlan` named `cluster`, or
  pin `spec.talosVersion` / `spec.kubernetesVersion`. For etcd snapshots
  before control-plane steps, create the Secret named in
  `upgrade.etcdSnapshotSecret` (keys `endpoint`, `bucket`, `accessKey`,
  `secretKey`, `region`, `prefix`), or set `spec.etcdSnapshotPolicy: disabled`.
- Config: create the Secret named in `config.gitSecretName` (keys `identity`,
  `known_hosts`) and a `MachineConfigPlan` named `cluster`. It starts in
  `DiffOnly` and applies nothing until `spec.mode: Reconcile`.

## Values

| Value | Default | |
|---|---|---|
| `image.repository` / `image.tag` | `ghcr.io/openresearch/talos-lifecycle`, chart `appVersion` | amd64 and arm64 |
| `upgrade.enabled` / `config.enabled` | `true` / `true` | each controller is its own Deployment |
| `upgrade.etcdSnapshotSecret` | `talos-lifecycle-s3` | the credential only needs `PutObject` on its prefix |
| `upgrade.installerImageBase` | stock Talos installer (Image Factory, no extensions) | for nodes not installed from an Image Factory image; `spec.installerImageBase` overrides it |
| `upgrade.releaseFeeds.*` | GitHub releases API | any feed in the same format |
| `config.gitSecretName` | `talos-config-git` | the only Secret the config controller can read |
| `crds.enabled` / `crds.keep` | `true` / `true` | CRDs upgrade with the chart and survive an uninstall |
| `metrics.enabled` | `true` | HTTPS on 8443; bind `<release>-metrics-reader` to your scraper |
| `serviceMonitor.enabled` / `prometheusRule.enabled` | `false` | need the Prometheus Operator CRDs |
| `priorityClassName` | `system-cluster-critical` | keeps the descheduler off the controllers |
| `affinity` | the two controllers on different nodes | |

## Security model

Both controllers hold `os:admin` Talos credentials, which can fetch an admin
kubeconfig: treat whoever can create pods or read Secrets in the release
namespace as cluster-admin. Within that, each controller has its own
ServiceAccount and Talos credential. The config controller reads exactly one
Secret by name. The upgrade controller can evict pods only in its own namespace
(itself, before upgrading its own node). Both run as non-root with a read-only
root filesystem and the Pod Security `restricted` profile.

## Uninstall

`helm uninstall` removes the controllers and keeps the CRDs, and with them your
`UpgradePlan` and `MachineConfigPlan`. Delete the CRDs by hand to remove those.
