# Issue #12: Peer list not updated when node IPs change until healthy node restarts

## Analysis Date: 2026-02-05

## Problem Summary
When node IPs change (e.g., pod rescheduling), some nodes fail to find peers to sync. Restarting the failing node doesn't help, but restarting the **healthy** peer node makes synchronization work immediately.

## Root Cause Analysis

### How Peer Configuration Works

1. **Peer Discovery (`getChainPeers()` in configmap.go)**:
   - Lists all Services with labels `peer=true` and `chain-id=<chain-id>`
   - Returns peers with the **service name** as the address (e.g., `mynode-internal`)
   - This is written to `config.toml` as `persistent_peers`

2. **Internal Service (`getInternalServiceSpec()` in service.go)**:
   - Creates a ClusterIP service with `PublishNotReadyAddresses: true`
   - Labels: `peer=true`, `chain-id`, `node-id`, `validator`, `seed`
   - Selector targets the specific ChainNode's pod

3. **Cosmos Node DNS Resolution**:
   - CometBFT/Tendermint resolves peer DNS names to IPs **at startup only**
   - Once resolved, the IP is cached in the P2P address book
   - The node does NOT re-resolve DNS during normal operation
   - P2P connections use the cached resolved IP directly

### The Bug Sequence

1. **Initial State**: Node A and Node B are peers, both healthy
2. **Pod Reschedule**: Node B's pod gets rescheduled, gets a new IP
3. **Stale Cache**: Node A still has Node B's OLD IP cached in its P2P layer
4. **Connection Failure**: Node A cannot connect to Node B (wrong IP)
5. **Restart A Doesn't Help**: 
   - Node A restarts and re-resolves DNS
   - Gets Node B's service ClusterIP (stable) → routes to new pod IP
   - BUT Node B still has Node A cached at some stale state
   - Handshake fails due to connection state mismatch
6. **Restart B Fixes It**:
   - Node B restarts fresh with no cached P2P state
   - Re-resolves all peer addresses
   - Accepts new incoming connections cleanly
   - Node A can now connect successfully

### Why the Controller Doesn't Detect This

1. **No Endpoint Watch**: The controller only watches:
   - `ChainNode` (CR changes)
   - `Pod` (owned pods)
   - `ConfigMap` (owned configmaps)
   - `Service` (owned services)
   
2. **No EndpointSlice Watch**: Endpoint changes are NOT watched

3. **ConfigMap Uses Service Names**: Since peer addresses are service names (not IPs), the ConfigMap content never changes when underlying endpoints change

4. **No Reconciliation Trigger**: Without config changes, no reconciliation happens → no pod restart → stale P2P cache persists

## Solution Design

### Chosen Approach: Watch EndpointSlices and Trigger Reconciliation

When peer endpoints change, we need to signal the Cosmos node to refresh its P2P connections. Since Cosmos nodes don't support hot-reload of peer configuration, the reliable solution is to trigger a pod restart when peer endpoints change.

#### Implementation Steps:

1. **Add EndpointSlice Watch** (controller.go):
   - Watch EndpointSlices in the cluster
   - Filter for EndpointSlices belonging to peer services (services with `peer=true` label)
   - When endpoints change, find ChainNodes that use this peer (same chain-id)
   - Enqueue those ChainNodes for reconciliation

2. **Track Peer Endpoint State** (configmap.go):
   - Calculate a hash of current peer endpoint addresses
   - Store hash in ConfigMap annotation: `cosmopilot.voluzi.com/peer-endpoints-hash`
   - When hash changes, ConfigMap updates → pod spec hash changes → pod recreated

3. **Pod Restart on Endpoint Change**:
   - The existing `AnnotationConfigHash` mechanism will trigger pod recreation
   - New pod starts fresh with empty P2P address book
   - Fresh DNS resolution gets correct peer IPs

#### Alternative Considered: Headless Services
Using headless services (ClusterIP: None) would make DNS return actual Pod IPs directly. However:
- Still has DNS caching issue in Cosmos nodes
- Would require migration of existing deployments
- More complex service topology

### Code Changes Required

1. **`internal/controllers/chainnode/controller.go`**:
   - Add EndpointSlice watch with custom handler
   - Handler maps EndpointSlice → Service → ChainNodes (via chain-id label)

2. **`internal/controllers/chainnode/configmap.go`**:
   - Add function to calculate peer endpoints hash
   - Include hash in ConfigMap annotations
   - Hash changes trigger ConfigMap update → pod restart

3. **`internal/controllers/chainnode/predicate.go`** (optional):
   - Add predicate for EndpointSlice events

## Implementation Progress

### Iteration 1 (Current)
- [x] Analyze codebase and understand peer discovery mechanism
- [x] Identify root cause: stale P2P cache + no endpoint watch
- [x] Design solution: EndpointSlice watch + endpoint hash annotation
- [ ] Implement EndpointSlice watch in controller.go
- [ ] Implement peer endpoints hash calculation
- [ ] Add hash to ConfigMap annotations
- [ ] Test changes

### Files to Modify
1. `internal/controllers/chainnode/controller.go` - Add EndpointSlice watch
2. `internal/controllers/chainnode/configmap.go` - Add endpoint hash calculation
3. `internal/controllers/chainnode/service.go` - Possibly add endpoint helper functions

## Testing Strategy

1. **Unit Tests**: 
   - Test endpoint hash calculation
   - Test EndpointSlice → ChainNode mapping

2. **Integration Tests**:
   - Deploy two ChainNodes as peers
   - Delete one pod (simulate reschedule)
   - Verify the other node's pod is restarted
   - Verify P2P connectivity restored

3. **Manual Testing**:
   - Use existing cluster with cosmopilot
   - Trigger pod reschedule via `kubectl delete pod`
   - Monitor peer connectivity
