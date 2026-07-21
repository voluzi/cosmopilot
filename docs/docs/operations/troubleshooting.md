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

## TMKMS / Vault issues (deprecated)

- Ensure the Vault token has permission for the operations you enabled (including key
  upload when `uploadGenerated` is set).
- For legacy TMKMS deployments using renewable or periodic tokens, `autoRenewToken` enables the
  deprecated `vault-token-renewer` sidecar. Migrate to [Cosmosigner](../usage/cosmosigner), which
  renews Vault tokens internally. See [TMKMS](../usage/tmkms).

## Leader election / multiple managers

When running more than one replica, leader election ensures only one manager is
active. The lease ID is derived from the release (and worker) name. If reconciles seem
to stop, check that a leader holds the lease and inspect the manager logs of all
replicas. See the [CLI reference](../reference/cli#manager).

## Still stuck?

Open an issue at
[github.com/voluzi/cosmopilot/issues](https://github.com/voluzi/cosmopilot/issues)
with the resource definition, relevant events, and operator logs.
