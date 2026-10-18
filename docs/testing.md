# How we test

A bad config or a failed upgrade can strand a node, and the way back is a
console. The tests that matter prove that a node recovers when something goes
wrong. Most of those failures cannot be produced on demand against a real
cluster, so they are tested against a fake, and the fake is checked against a
real cluster.

## In this repository

```sh
make test   # unit and envtest suites
make lint
```

The config controller's tests run against `confignode.FakeNode`, which models
the two Talos config slots and the revert timer with an injected clock. A node
that drops off the network mid-try and comes back ten minutes later costs no
wall time. The upgrade controller's Talos calls sit behind an interface for the
same reason. The suites cover:

- **Crash safety.** A restart at any point never sends an upgrade, rollback or
  config apply twice.
- **Failure paths.** Unreachable node, rejected config, the revert timer
  beating the commit, a reboot mid-try, a Git push during the soak, a refused
  upgrade image, a node deleted mid-upgrade.
- **Outages.** A controller outage longer than a phase timeout does not fail
  a healthy node: the node is asked before the timeout is judged.
- **Gates.** etcd voters, apiservers, Talos and Kubernetes compatibility,
  windows, cooldowns. A gate that cannot decide blocks.
- **Secrets.** Never appear in status or logs.

## On a real cluster

Every release runs against a dev cluster of three control-plane and three
worker nodes on VMs:

- **Upgrade walk.** The cluster is rebuilt at old versions (last: Talos 1.12 /
  Kubernetes 1.31) and walks to the latest releases by itself, with a config
  change rolling out alongside. During the walk the controller is killed in
  the middle of an Upgrade call, and a rollback is forced by failing a node's
  health check.
- **Config cases.** A config that takes a node off the network, the controller
  killed mid-try, Git down mid-try, a push during the soak, a reboot-only
  field, a closed window, a nonexistent Talos image, and the RBAC each
  ServiceAccount actually has.
- **Invariant watcher.** Runs read-only throughout and fails the run on any
  breach: more than one node in flight, both controllers in flight at once, a
  failed node without a frozen plan, a config persisted before it was verified,
  a node NotReady for over 20 minutes, an unsupported Talos/Kubernetes pair.

The stranded-node case is the one the config controller exists for: a worker
given a static address on a subnet it is not on went unreachable for over two
minutes, the controller sent nothing, and the node restored its previous
config on its own within seconds of its revert deadline.

Cluster runs find what the fake cannot. Examples fixed this way: a node
rebooted by a Talos upgrade has no persisted config slot until the next
persisting apply; a controller restart during the Upgrade call can make Talos
drop the upgrade; the GitHub releases API pages its results, so reading one
page stalled the version walk.
