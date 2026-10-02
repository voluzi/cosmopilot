# Troubleshooting

This page collects common issues and how to diagnose them. `Cosmopilot` records
Kubernetes **Events** on the resources it manages, so the first step for almost any
problem is to read them.

## General diagnosis

Start with the resource status and events:

```bash
# High-level status
kubectl get chainnode <name> -o wide
kubectl describe chainnode <name>

# Recent events in the namespace, newest last
kubectl get events --sort-by=.lastTimestamp
```

Then look at the Pod and its containers:

```bash
kubectl describe pod <node-pod>
kubectl logs <node-pod> -c app            # the chain binary
kubectl logs <node-pod> -c node-utils     # the operator sidecar
```

And the operator itself:

```bash
kubectl logs deploy/cosmopilot -n cosmopilot-system
```

:::tip
Enable verbose operator logs with `--set debugMode=true` when reproducing an issue.
:::

## Webhook / admission errors

**Symptom:** creating a `ChainNode`/`ChainNodeSet` fails with a webhook error, or
nothing happens and the manager logs mention certificates.

`Cosmopilot`'s admission webhooks require valid serving certificates, normally issued
by [cert-manager](https://cert-manager.io/). If cert-manager is not installed:

- install cert-manager **before** installing `Cosmopilot`, or
- disable webhooks with `--set webHooksEnabled=false`.

See [Installation](../getting-started/installation) and
[Prerequisites](../getting-started/prerequisites).

## Pod keeps restarting after a config change

`Cosmopilot` stores a hash of the rendered configuration in the
`cosmopilot.voluzi.com/config-hash` annotation. When configuration changes, the Pod is
restarted intentionally to apply it. If a Pod restarts unexpectedly, compare the
annotation before/after and check the operator logs for the reconcile that triggered
it. See [Annotations](../reference/annotations#annotations).

## Image won't pull

**Symptom:** the Pod is stuck in `ImagePullBackOff` or `ErrImagePull`.

- Verify the node image and tag are correct and reachable.
- For private registries, set `imagePullSecrets` (see
  [Configuration](../getting-started/configuration)).
- Remember `Cosmopilot` enforces the **restricted** Pod Security profile — the image
  must run as non-root. See [Prerequisites](../getting-started/prerequisites#container-image-requirements).

## Node not syncing / height not advancing

- Check connected peers — a node with no peers cannot sync. Confirm peering and any
  `persistentPeers`/seeds.
- Check the `app` container logs for consensus or networking errors.
- If you expect state-sync, confirm the configured trust height/hash and that RPC
  servers are reachable. The operator tracks state-sync via the
  `cosmopilot.voluzi.com/state-sync-trust-height` / `-trust-hash` annotations.
- If using `blockThreshold`, a stalled node may be marked unhealthy by `node-utils`.

## Snapshot or restore problems

- **Integrity check fails:** when `verify` is enabled, `Cosmopilot` starts a temporary
  node from the snapshot; if it fails, the snapshot is deleted and a new one is taken.
  Repeated failures usually point to corrupted data or insufficient resources for the
  verification Pod. See [Persistence & Backup](../usage/persistence-and-backup#integrity-checks).
- **Restore not starting:** confirm the source snapshot/tarball exists and that the
  storage class supports the snapshot data source. See
  [Restoring from Snapshot](../usage/restoring-from-snapshot).

## PVC not resizing

`Cosmopilot` auto-resizes a node's PVC when usage crosses the configured threshold,
but this requires a storage class with `allowVolumeExpansion: true`. If volumes don't
grow, verify the storage class supports expansion. See
[Persistence & Backup](../usage/persistence-and-backup).

## Profile node-utils memory or CPU

Builds containing the profiling endpoint serve Go runtime profiles on
`127.0.0.1:6666` inside the Pod. Forward that loopback port when diagnosing the
`node-utils` sidecar:

> **Warning:** TCP port `6666` is reserved for node-utils profiling. Do not
> configure the Cosmos application or other containers in the same Pod to use it.
> The node-utils sidecar can bind it before the application starts, causing a
> conflicting application listener to fail. The profiling port is fixed; it does
> not automatically switch to another port.

```bash
kubectl port-forward pod/<node-pod> 6666:6666
```

Keep the port-forward running. In another terminal, collect and inspect the profiles:

```bash
curl -o heap.pb.gz http://127.0.0.1:6666/debug/pprof/heap
curl -o cpu.pb.gz 'http://127.0.0.1:6666/debug/pprof/profile?seconds=10'
curl -o trace.out 'http://127.0.0.1:6666/debug/pprof/trace?seconds=5'
go tool pprof -top heap.pb.gz
go tool pprof -top cpu.pb.gz
go tool trace trace.out
```

Execution traces use Go's trace format and `go tool trace`; CPU and heap
profiles use `go tool pprof`.

These profiles describe the `node-utils` Go process, not the Cosmos process or
total Pod memory. The port is loopback only and is not exposed by a Service or
Ingress, but other containers in the same Pod can reach it. Profiles may contain
sensitive process data; keep access to the Pod and port-forward controlled.
A CPU profile or execution trace collects data only while requested;
block and mutex sampling remain at Go's defaults. If port 6666 is already occupied
when the profiling listener starts,
`node-utils` logs a warning and continues without profiling.
If the node-utils primary API is configured on port 6666, that API keeps the port;
`node-utils` logs a warning and disables profiling.

## Cosmosigner startup and Vault issues

- Check the signer startup, liveness (`/livez`), and readiness (`/readyz`) probes on HTTP port 8080.
  Readiness includes backend and preflight initialization; followers are ready too.
- Ensure the Vault token has the [required permissions](../usage/cosmosigner#vault-transit),
  including cluster-binding registry access. Cosmosigner renews renewable and periodic tokens itself.
- If `wait-cosmosigner-discovery` times out after 25 seconds, inspect its logs for DNS diagnostics,
  the discovery Service endpoints, the signer leader logs, and NetworkPolicy enforcement on TCP 26659.
  A bare TCP connection does not release the gate; it must receive data from the signer.
- Repeated `can't get pubkey` exits after the gate passes require investigation. Verify Cosmosigner
  3.1.0 or newer and node-utils 4.0.0 or newer, including any image overrides.
- A legacy signing-path refusal requires completing the migration on 4.x; follow the
  [5.0.0 upgrade guide](../getting-started/upgrading-to-5).

## Leader election / multiple managers

When running more than one replica, leader election ensures only one manager is
active. The lease ID is derived from the release (and worker) name. If reconciles seem
to stop, check that a leader holds the lease and inspect the manager logs of all
replicas. See the [CLI reference](../reference/cli#manager).

## Still stuck?

Open an issue at
[github.com/voluzi/cosmopilot/issues](https://github.com/voluzi/cosmopilot/issues)
with the resource definition, relevant events, and operator logs.
