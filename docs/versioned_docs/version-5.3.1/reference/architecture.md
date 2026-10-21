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
- gracefully shutting the node down for snapshots.

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

### Cosmosigner (optional)

[Cosmosigner](../usage/cosmosigner) runs in a separate Raft StatefulSet and dials each target node's
privval listener on TCP 26659. A target NetworkPolicy allows that port only from the associated
signer pods and preserves ingress on every other TCP port and all UDP/SCTP ports. The CNI must
enforce NetworkPolicy and support `endPort`.

The target's final init container listens on the same privval port, reads the first byte from a
signer connection, then closes it and exits. The app starts its listener and the signer redials
directly. The node-utils sidecar continues its other duties. Signer HTTP probes use `/livez` and
`/readyz` on port 8080; no Service exposes that port.

### dataexporter (job)

A CLI tool used to stream snapshot tar archives to Google Cloud Storage, Amazon
S3, and S3-compatible object stores, and to delete them. It supports uncompressed,
gzip, zstd, and lz4 archives and runs as a short-lived job during snapshot export.
See the
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
- **`wait-cosmosigner-discovery`** — the final init container for remote signer targets.

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
