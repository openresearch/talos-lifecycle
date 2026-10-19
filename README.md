# talos-lifecycle

Two Kubernetes controllers for Talos Linux clusters, shipped as one image:

- **Upgrade controller** (`UpgradePlan`): upgrades Talos node by node and
  Kubernetes cluster-wide. Control-plane nodes go first, one node at a time,
  behind etcd quorum and cluster health gates, with an etcd snapshot before
  each control-plane step and automatic rollback of a node that fails its
  health check.
- **Config controller** (`MachineConfigPlan`): rolls Talos machine config out
  from a Git repository with try-verify-commit. Each node runs the new config
  in `TRY` mode on its own revert timer, is probed and soaked, and only then
  is the config persisted.

Both plans are cluster-scoped singletons named `cluster`. Any failure freezes
the plan until an operator acknowledges it. The two controllers never work on
the cluster at the same time: each waits while the other has a node in flight.

## Prerequisites

- Talos with the Talos API reachable from pods. Allow `os:admin` for the
  release namespace on the control-plane nodes (Talos 1.14 and later):

  ```yaml
  apiVersion: v1alpha1
  kind: KubeTalosAPIAccessConfig
  allowedRoles:
    - os:admin
  allowedKubernetesNamespaces:
    - talos-lifecycle-system
  ```

  Older Talos versions use `machine.features.kubernetesTalosAPIAccess`
  (`enabled: true` plus the same two lists).

- Both controllers need `os:admin`: Talos requires it for `Upgrade`,
  `Rollback` and `ApplyConfiguration`. An `os:admin` credential can fetch an
  admin kubeconfig, so treat anyone who can create pods or read Secrets in the
  release namespace as cluster-admin.

## Install

With Helm:

```sh
kubectl create namespace talos-lifecycle-system
kubectl label namespace talos-lifecycle-system pod-security.kubernetes.io/enforce=restricted
helm install talos-lifecycle oci://ghcr.io/openresearch/charts/talos-lifecycle \
  --version <version> -n talos-lifecycle-system
```

Values and the security model: [charts/talos-lifecycle/README.md](charts/talos-lifecycle/README.md).

With plain manifests, built from this repository:

```sh
make build-installer build-installer-config IMG=<registry>/talos-lifecycle:<tag>
kubectl apply -f dist/install.yaml            # namespace, CRDs, upgrade controller
kubectl apply -f dist/config-controller.yaml  # config controller
```

## Upgrades

Create the `UpgradePlan`:

```yaml
apiVersion: talos.openresearch.com/v1alpha1
kind: UpgradePlan
metadata:
  name: cluster
spec:
  channel: manual          # or latest: the controller advances one release at a time
  talosVersion: v1.14.2    # with manual, setting a version is the approval
  kubernetesVersion: v1.37.1
  cooldownMinutes: 60      # pause between two upgrade steps
  # window: {start: "20:00", end: "05:00"}
```

With `channel: latest` leave both versions unset; the controller walks to the
newest stable release, latest patch first, never skipping a minor.

etcd snapshots go to S3 before every control-plane step. Create the Secret, or
set `spec.etcdSnapshotPolicy: disabled`:

```sh
kubectl -n talos-lifecycle-system create secret generic talos-lifecycle-s3 \
  --from-literal=endpoint=https://s3.example.com --from-literal=bucket=etcd-snapshots \
  --from-literal=accessKey=... --from-literal=secretKey=... \
  --from-literal=region=... --from-literal=prefix=my-cluster
```

The credential only needs `PutObject` on that prefix.

Upstream versions come from the GitHub releases API by default (60
unauthenticated requests per hour, one poll a day). Point `TALOS_RELEASE_URL`
and `KUBERNETES_RELEASE_URL` (chart: `upgrade.releaseFeeds`) at a mirror in the
same format if needed.

## Machine config

The config tree in Git holds Talos patches, applied onto each node's own
config in this order:

```
base/*.yaml            every node
base/<role>/*.yaml     every node of a role, instead of base/*.yaml
roles/<role>.yaml      every node of a role
nodes/<name>.yaml      one node
```

Roles are `control-plane` and `worker`. Every file is a strategic merge or
JSON6902 patch, as `talosctl patch machineconfig` takes it. The tree holds only
the fields it manages and no secrets. Three rules:

- Removing a line from the tree does not remove it from the node; use a patch
  that deletes it.
- Strategic merge appends to lists, so list fields go in JSON6902 `add` patches.
  The renderer refuses a tree that would change on a second apply.
- Fields Talos reads only at boot are not applied; they keep the node's value
  and are reported as skipped.

Create the Git credential (the config controller can read only this Secret):

```sh
kubectl -n talos-lifecycle-system create secret generic talos-config-git \
  --from-file=identity=./deploy_key --from-file=known_hosts=./known_hosts
```

Then the plan:

```yaml
apiVersion: talos.openresearch.com/v1alpha1
kind: MachineConfigPlan
metadata:
  name: cluster
spec:
  mode: DiffOnly           # report only; Reconcile applies
  source:
    git:
      url: ssh://git@git.example.com/infra/talos-config.git
      ref: main
      path: clusters/prod
      secretRef:
        name: talos-config-git
        namespace: talos-lifecycle-system
  cooldownMinutes: 5
```

Start in `DiffOnly` and check `status.nodes`: switch to `Reconcile` once the
diff is empty, which proves the tree reproduces what the nodes run.

## Operating

```sh
kubectl get upgradeplan cluster -o yaml        # phase, reason, per-node progress
kubectl get machineconfigplan cluster -o yaml
```

A failed node freezes its plan. Fix the cause, then acknowledge with the node
name or `all` (on the `UpgradePlan` also `kubernetes`):

```sh
kubectl annotate upgradeplan cluster talos.openresearch.com/unfreeze=worker-1
kubectl annotate machineconfigplan cluster talos.openresearch.com/unfreeze=all
```

The controller removes the annotation once it has acted on it.
`spec.paused: true` stops the config controller from starting new nodes.

Metrics are served over HTTPS on port 8443. Prometheus Operator objects
(`ServiceMonitor`, `PrometheusRule`) are in the chart, off by default.

## Development

```sh
make test        # unit and envtest tests
make lint
make build       # bin/manager
make chart-check # the chart must render the same objects as the Kustomize install
```

CI (`.github/workflows/ci.yml`) runs the tests, lint and chart checks on every
push to `main` and every pull request. Pushing a tag `vX.Y.Z` runs
`.github/workflows/release.yml`: goreleaser builds and pushes the amd64/arm64
image to `ghcr.io/openresearch/talos-lifecycle`, the chart goes to
`oci://ghcr.io/openresearch/charts/talos-lifecycle`, and a GitHub release gets
`install.yaml`, `config-controller.yaml` and the chart package.

`make help` lists every target. A new Kubernetes or Talos minor needs a
dependency bump of `github.com/siderolabs/*` and a rebuild: the supported
upgrade paths are compiled in.

## Maintainers

This project is maintained by OpenResearch GmbH. We use AI-assisted
engineering tools to accelerate development; all code is reviewed, tested and
verified by our engineering team for security and correctness.

## License

Copyright OpenResearch GmbH. Licensed under the Apache License, Version 2.0;
see [LICENSE](LICENSE).
