# Annotations, Labels & Ports

This is a reference for the Kubernetes annotations and labels `Cosmopilot` sets on the
resources it manages, and for the ports used by nodes and their sidecars. It is useful
for inspecting operator state, writing selectors, and configuring monitoring or
network policies.

## Annotations

All `Cosmopilot` annotations use the `cosmopilot.voluzi.com/` prefix and are
**managed by the operator** — they reflect internal state and should be treated as
read-only. Inspecting them is a quick way to understand what the operator is doing
with a node.

| Annotation | Set on | Meaning |
| --- | --- | --- |
| `cosmopilot.voluzi.com/state-sync-trust-height` | Node | Trusted block height used for state-sync. |
| `cosmopilot.voluzi.com/state-sync-trust-hash` | Node | Trusted block hash used for state-sync. |
| `cosmopilot.voluzi.com/data-height` | Node / PVC | Block height of the data currently on disk. |
| `cosmopilot.voluzi.com/data-initialized` | Node | Marks that the data volume has been initialized. |
| `cosmopilot.voluzi.com/genesis-downloaded` | Node | Marks that the genesis file has been retrieved. |
| `cosmopilot.voluzi.com/vault-key-uploaded` | Node | Marks that the consensus key has been uploaded to Vault. |
| `cosmopilot.voluzi.com/config-hash` | Pod | Hash of the rendered configuration; a change triggers a controlled Pod restart. |
| `cosmopilot.voluzi.com/pod-spec-hash` | Pod | Hash of the desired Pod spec, used to detect drift. |
| `cosmopilot.voluzi.com/snapshotting-pvc` | Node | A PVC snapshot is currently in progress. |
| `cosmopilot.voluzi.com/last-pvc-snapshot` | Node | Timestamp/reference of the last PVC snapshot. |
| `cosmopilot.voluzi.com/snapshot-ready` | VolumeSnapshot | The snapshot is ready to use. |
| `cosmopilot.voluzi.com/snapshot-retention` | VolumeSnapshot | Retention marker for the snapshot. |
| `cosmopilot.voluzi.com/snapshot-integrity-status` | VolumeSnapshot | Result of the snapshot integrity check. |
| `cosmopilot.voluzi.com/exporting-tarball` | Node | A snapshot tarball export is in progress. |
| `cosmopilot.voluzi.com/vpa-resources` | Pod | Resources currently applied by the vertical autoscaling logic. |
| `cosmopilot.voluzi.com/last-cpu-scale` | Pod | Timestamp of the last CPU scaling action. |
| `cosmopilot.voluzi.com/last-memory-scale` | Pod | Timestamp of the last memory scaling action. |
| `cosmopilot.voluzi.com/oom-recovery-history` | Pod | History used to recover from out-of-memory events. |

### Standard Kubernetes annotations

`Cosmopilot` also relies on a couple of well-known Kubernetes annotations:

| Annotation | Meaning |
| --- | --- |
| `cluster-autoscaler.kubernetes.io/safe-to-evict` | Controls whether the cluster autoscaler may evict a node Pod. The operator manages this to avoid disrupting nodes at the wrong time. |
| `statefulset.kubernetes.io/pod-name` | Standard pod-name label/annotation used when binding storage. |

## Labels

`Cosmopilot` applies these labels to Pods, Services and other resources. They are
useful for `kubectl` selectors, monitoring selectors and network policies. Labels you
set on a `ChainNode` are also propagated to its Service.

| Label | Value | Meaning |
| --- | --- | --- |
| `node-id` | string | The node's Tendermint/CometBFT node ID. |
| `chain-id` | string | The chain ID the node belongs to. |
| `chain-node` | string | Name of the owning `ChainNode`. |
| `nodeset` | string | Name of the owning `ChainNodeSet` (when applicable). |
| `group` | string | Group name within a `ChainNodeSet`. |
| `validator` | `true` / `false` | Whether the node is a validator. |
| `seed` | `true` / `false` | Whether the node is a seed. |
| `peer` | `true` / `false` | Whether the node participates as a peer. |
| `scope` | string | Resource scope marker used by the operator. |
| `app` | string | Application label. |
| `global-ingress` | string | Marks resources belonging to a global ingress. |
| `upgrading` | `true` | Present on a Pod while it is being upgraded. |
| `worker-name` | string | Which operator worker owns this resource (for [sharding](../getting-started/configuration#worker-configuration)). |

## Ports

### Node ports

These ports are exposed on the node Pod and its Service.

| Port name | Port | Description |
| --- | --- | --- |
| `p2p` | `26656` | CometBFT peer-to-peer. |
| `rpc` | `26657` | CometBFT RPC. |
| `lcd` | `1317` | Cosmos SDK REST (LCD) API. |
| `grpc` | `9090` | Cosmos SDK gRPC. |
| `prometheus` | `26660` | Node Prometheus metrics (see [Monitoring](../usage/monitoring)). |
| `privvalidator` | `26659` | Private validator listen address. |
| `node-utils` | `8000` | Internal `node-utils` sidecar API (operator use only). |

For EVM-enabled chains, the following are added:

| Port name | Port | Description |
| --- | --- | --- |
| `evm-rpc` | `8545` | EVM JSON-RPC. |
| `evm-rpc-ws` | `8546` | EVM JSON-RPC over WebSocket. |

### CosmoGuard ports

When [CosmoGuard](../usage/cosmoguard) is enabled, a standalone CosmoGuard Deployment
fronts the node's APIs. Its container and `<name>-cosmoguard` Service listen on these
ports (the group/global Services keep the public port numbers and target these):

| Port name | Port | Fronts |
| --- | --- | --- |
| `fw-rpc` | `16657` | RPC. |
| `fw-lcd` | `11317` | LCD. |
| `fw-grpc` | `19090` | gRPC. |
| `fw-evm-rpc` | `18545` | EVM RPC. |
| `fw-evm-rpc-ws` | `18546` | EVM RPC WebSocket. |
| `fw-metrics` | `9001` | CosmoGuard's own metrics. |

### Manager ports

| Port | Description |
| --- | --- |
| `8080` | Operator metrics. |
| `8081` | Health (`/healthz`) and readiness (`/readyz`) probes. |
| `9443` | Admission webhook server. |
