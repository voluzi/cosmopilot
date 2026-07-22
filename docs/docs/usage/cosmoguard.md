# Using CosmoGuard

[CosmoGuard](https://github.com/voluzi/cosmoguard) is a lightweight firewall designed specifically for protecting Cosmos nodes. With CosmoGuard you can control access at the API endpoint level, cache responses for performance, rate-limit clients, and limit WebSocket connections for better resource management.

`Cosmopilot` integrates with CosmoGuard **v4** and deploys it as a **standalone clustered StatefulSet** that sits in front of your node(s), rather than as a sidecar container inside the node pod.

## Topology

```text
client traffic
  -> Service / Ingress / Gateway
  -> CosmoGuard StatefulSet       (clustered shared cache, scalable, HPA-capable)
  -> node pods                    (discovered via a headless Service)
```

- On a **`ChainNodeSet`**, Cosmopilot deploys **one CosmoGuard StatefulSet per node group**, fronting every node in that group. It can run multiple replicas and be autoscaled with an HPA.
- On a standalone **`ChainNode`**, Cosmopilot deploys a single CosmoGuard StatefulSet fronting that node.
- The node's main and `-internal` Services keep serving the raw node ports. Guarded traffic is routed through the group/global Services (whose selectors are flipped to the guard once it is ready) and through the dedicated `<name>-cosmoguard` Service.

### Shared cache (olric cluster)

CosmoGuard runs as a StatefulSet so every replica joins one **embedded olric cache cluster** and shares a single distributed cache. A response cached by any replica is served from cache by all of them, so the backing nodes are shielded no matter how the load balancer spreads requests — this is the whole point of running multiple replicas in front of a group. Cosmopilot wires this automatically:

- a **headless peer Service** (`<name>-cosmoguard-peer`) gives each replica stable DNS for olric's peer discovery;
- gossip traffic is encrypted with a key Cosmopilot generates once into a **Secret** (`<name>-cosmoguard-cluster`) and mounts into every replica.

You don't configure any of this — enabling CosmoGuard is enough.

:::info[Migrating from the sidecar model]
Earlier releases ran CosmoGuard as a sidecar container inside each node pod. Enabling CosmoGuard no longer modifies the node pod. When you upgrade, Cosmopilot brings the standalone guard up first and only routes traffic through it once it is ready (make-before-break), then recreates the node pods without the sidecar. Your rules `ConfigMap` is never modified.
:::

## Why Use CosmoGuard?

- **Fine-Grained API Access Control:** Manage access on a per-endpoint level (RPC, LCD, gRPC, EVM).
- **Performance Optimization with Caching:** Cache frequently accessed responses (in-memory; no external cache needed for a single replica).
- **Rate Limiting & WebSocket Management:** Protect nodes from overload.
- **Independent Scaling:** Scale the guard independently of the nodes, with optional autoscaling.
- **Hot-Reloading:** Rule changes in the `ConfigMap` are hot-reloaded without a restart.

## Setting Up CosmoGuard

### Step 1: Create the CosmoGuard rules

Create a configuration file containing **only rules** following CosmoGuard's [config structure](https://github.com/voluzi/cosmoguard/blob/main/CONFIG.md). An example allowing only the `/status` endpoint on RPC and caching its response:

```yaml
cache:
  ttl: 10s

rpc:
  rules:
    - action: allow
      match:
        path: /status
        method: GET
      cache:
        enable: true
```

:::warning[IMPORTANT]
Provide **rules only** in your `ConfigMap`. Cosmopilot manages the upstream (node discovery), listener ports, metrics and dashboard settings through environment variables — do not set them in the file.

CosmoGuard v4 **removed Redis**: a `cache.backend`, `cache.redis` or `cache.redis-sentinel` key now fails startup. For multi-replica caches CosmoGuard uses an embedded cluster; single-replica needs no cache backend at all. See the CosmoGuard [v4 migration notes](https://github.com/voluzi/cosmoguard/blob/main/CONFIG.md) for other breaking changes (WebSocket cross-origin now denied by default, CosmoGuard owns CORS, gRPC reflection is no longer auto-allowed). You can validate a file with `cosmoguard validate <file>`.
:::

### Step 2: Create a ConfigMap in Kubernetes

```bash
kubectl create configmap cosmoguard-config --from-file=cosmoguard.yaml=/path/to/your/cosmoguard.yaml -n <namespace>
```

### Step 3: Enable CosmoGuard

To enable CosmoGuard for a `ChainNode` or a node group within a `ChainNodeSet`, add the following to the node/group `config`:

```yaml
config:
  cosmoGuard:
    enable: true
    config:
      name: cosmoguard-config  # Name of the ConfigMap created in Step 2.
      key: cosmoguard.yaml     # Key within the ConfigMap containing the rules.
    replicas: 2                # Optional: number of CosmoGuard replicas (default 1). Ignored when autoscaling is enabled.
    image: ghcr.io/voluzi/cosmoguard:4.0.0  # Optional: override the operator-wide default image.
    resources:                 # Optional: per-pod resources (defaults shown).
      requests:
        cpu: 200m
        memory: 250Mi
      limits:
        cpu: 200m
        memory: 250Mi
```

:::note
`restartPodOnFailure` is deprecated and has no effect: CosmoGuard now runs as a standalone StatefulSet supervised by Kubernetes.
:::

## Autoscaling

Scale CosmoGuard independently of the nodes with a HorizontalPodAutoscaler:

```yaml
config:
  cosmoGuard:
    enable: true
    config:
      name: cosmoguard-config
      key: cosmoguard.yaml
    autoscaling:
      enable: true
      minReplicas: 2
      maxReplicas: 8
      targetCPUUtilizationPercentage: 75      # Optional (defaults to 80 when neither target is set).
      targetMemoryUtilizationPercentage: 70   # Optional.
```

When autoscaling is enabled the HPA owns the replica count (the `replicas` field is ignored).

## Dashboard

CosmoGuard ships a read-only web dashboard. Expose it (opt-in, off by default):

```yaml
config:
  cosmoGuard:
    enable: true
    config:
      name: cosmoguard-config
      key: cosmoguard.yaml
    dashboard:
      enable: true
      port: 8080                    # Optional (default 8080).
      basicAuth:                    # Optional: credentials sourced from a Secret (never inlined).
        username:
          name: cosmoguard-dashboard-auth
          key: username
        password:
          name: cosmoguard-dashboard-auth
          key: password
      ingress:                      # Optional: expose the dashboard through an Ingress.
        host: cosmoguard.example.com
        ingressClassName: nginx
        tlsSecretName: cosmoguard-dashboard-tls
```

## Customizing Rules

Refer to the [CosmoGuard repo](https://github.com/voluzi/cosmoguard) for detailed information on creating custom rules. A few tips:

- **Match expressively:** v4 supports an expressive `match` tree (`all`/`any`/`none` + `path`/`method`/`query`/`header`/`sourceIP`) with glob values.
- **Prioritize Rules:** lower `priority` numbers match first.
- **Enable Caching:** cache frequently requested endpoints to reduce node load.

Example rule allowing the `/block` endpoint with caching:

```yaml
rpc:
  rules:
    - action: allow
      match:
        path: /block/**
        method: GET
      cache:
        enable: true
        ttl: 15s
```
