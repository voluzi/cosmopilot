# Architecture

This page explains how `Cosmopilot` is put together: the components it ships, the
custom resources it reconciles, and what a running node actually looks like inside
your cluster. It is meant as a conceptual map — for field-level details see the
[CRDs reference](./crds), and for flags and ports see the
[CLI reference](./cli) and [Annotations & Ports reference](./annotations).

## Overview

`Cosmopilot` is a standard Kubernetes [operator](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/).
It watches two custom resources — `ChainNode` and `ChainNodeSet` — and continuously
reconciles the cluster state to match them. Everything a node needs (Pod, storage,
configuration, services, ingress, secrets) is created and kept in sync by the operator.

```
                    ┌──────────────────────────────┐
                    │      Cosmopilot manager       │
                    │  (Deployment, leader-elected) │
                    │                               │
                    │  • ChainNode controller       │
                    │  • ChainNodeSet controller    │
                    │  • Admission webhooks         │
                    └───────────────┬───────────────┘
                                    │ watches & reconciles
              ┌─────────────────────┼─────────────────────┐
              ▼                     ▼                     ▼
       ┌────────────┐        ┌────────────┐        ┌────────────┐
       │  ChainNode │        │  ChainNode │        │  ChainNode │   ← one Pod each
       │   Pod      │        │   Pod      │        │   Pod      │
       │ + PVC      │        │ + PVC      │        │ + PVC      │
       │ + Service  │        │ + Service  │        │ + Service  │
       └────────────┘        └────────────┘        └────────────┘
```

## Components

`Cosmopilot` is distributed as a Helm chart that installs a single **manager**
Deployment. The manager, in turn, deploys several helper components alongside each
node as needed.

### Manager

The operator process itself. It runs both controllers and the admission webhook
server in one binary:

- **ChainNode controller** — reconciles a single node: its Pod, PVC(s), Services,
  ConfigMaps, Secrets, ingress/gateway routes, snapshots and upgrades.
- **ChainNodeSet controller** — reconciles a set of nodes. It owns and manages
  `ChainNode` resources (one per instance, per group), plus group-level Services,
  ingresses and shared genesis ConfigMaps.
- **Admission webhooks** — validate `ChainNode` and `ChainNodeSet` resources on
  create/update (can be disabled with `webHooksEnabled=false`).

The manager exposes a metrics endpoint and health probes, and supports
leader election and worker sharding. See the
[CLI reference](./cli#manager) for the full list of flags and environment variables.

### node-utils (sidecar)

A small sidecar container (`node-utils`, image `ghcr.io/voluzi/node-utils`) that runs
in **every** node Pod. It exposes an internal HTTP API on port `8000` that the
operator uses to drive and observe the node, including:

- reporting data directory size (used for auto-resize decisions);
- reporting the latest block height and whether the node is state-syncing;
- detecting when a governance upgrade height has been reached;
- gracefully shutting the node down for snapshots;
- proxying the TMKMS connection when enabled.

This API is internal to the operator and is not meant to be consumed directly. See
[Monitoring & Observability](../usage/monitoring) for the node metrics you _can_ scrape.

### CosmoGuard (optional)

When API exposure with fine-grained access control and caching is enabled,
`Cosmopilot` deploys a standalone [CosmoGuard](https://github.com/voluzi/cosmoguard) **v4**
clustered StatefulSet (image `ghcr.io/voluzi/cosmoguard`) in front of the node's API
endpoints — one per node group on a `ChainNodeSet`, or one per standalone `ChainNode`. Its
replicas share one distributed (olric) cache, and it can be autoscaled. See
[CosmoGuard](../usage/cosmoguard).

### Cosmoseed (optional)

For dedicated seed nodes, `Cosmopilot` can deploy
[Cosmoseed](https://github.com/voluzi/cosmoseed) (image `ghcr.io/voluzi/cosmoseed`),
a lightweight seed-only implementation. See [Cosmoseed](../usage/cosmoseed).

### TMKMS & vault-token-renewer (deprecated, optional)

For legacy validators configured with deprecated [TMKMS](../usage/tmkms), a TMKMS container is
added to the validator Pod. If `autoRenewToken` is enabled, the deprecated
`vault-token-renewer` sidecar keeps the Vault token renewed. New deployments should use
[Cosmosigner](../usage/cosmosigner), which renews Vault tokens internally.

### dataexporter (job)

A CLI tool used to upload snapshot tarballs to external storage (Google Cloud
Storage) and to delete them. It runs as a short-lived job during snapshot export
rather than as a long-running process. See the
[CLI reference](./cli#dataexporter).

## Custom resources

| Resource | Scope | Purpose |
| --- | --- | --- |
| `ChainNode` | Single node | Deploy and manage one Cosmos node (full node, validator, sentry, or seed). |
| `ChainNodeSet` | Group of nodes | Deploy and manage multiple nodes organized into groups, with shared genesis, services and ingresses. |

A `ChainNodeSet` is essentially a higher-level resource that produces and owns
several `ChainNode` resources. Deleting the set cleans up the nodes it owns.

## Anatomy of a node Pod

Each `ChainNode` is backed by a single **Pod** (not a StatefulSet), so the operator
has fine-grained control over its lifecycle. A typical Pod contains:

- **`app`** — the chain binary itself (your node image).
- **`node-utils`** — the helper sidecar (always present).
- **`tmkms`** — deprecated remote signer for legacy validators.
- **`vault-token-renewer`** — deprecated Vault token renewer for legacy TMKMS configurations.

Init containers handle one-time setup (data initialization, genesis retrieval, key
provisioning) before the node starts.

Alongside the Pod, the controller manages:

- one or more **PVCs** for the node's data (with optional auto-resize);
- a **Service** exposing the node's ports (see [ports](./annotations#ports));
- **ConfigMaps** for `config.toml`, `app.toml` and other configuration;
- **Secrets** holding the node key and, when generated, the consensus and account keys;
- optional **Ingress**/**Gateway** routes when endpoints are exposed.

## Reconciliation flow

On every change to a `ChainNode` (and periodically), the controller runs an
idempotent reconcile that, broadly:

1. ensures the node's **Services** exist;
2. renders configuration and computes a **config hash** (changes to configuration
   trigger a controlled Pod restart);
3. ensures the **Pod** exists and matches the desired spec (including the config hash);
4. ensures **PVC** updates, such as auto-resize when usage crosses the configured
   threshold;
5. handles higher-level lifecycle: genesis retrieval/creation, data initialization,
   state-sync, scheduled and governance upgrades, and snapshots.

The operator records Kubernetes **Events** on the resources throughout this process,
which are a useful first stop when [troubleshooting](../operations/troubleshooting).

## Configuration & state tracking

`Cosmopilot` stores operational state on the resources it manages using
annotations (for example: data height, genesis-downloaded, config hash, snapshot
status, VPA scaling history). These are documented in the
[Annotations & Ports reference](./annotations) so you can inspect what the operator
is doing at any point.

## Running multiple operator instances (sharding)

A single manager can run all reconciles, but for large fleets you can run multiple
operator instances and shard the work between them using `workerName`. Each instance
only reconciles resources labelled for it, and `workerCount` controls how many
concurrent reconciles a single instance performs. See
[Configuration](../getting-started/configuration#worker-configuration).
