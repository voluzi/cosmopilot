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
- The node's main and `-internal` Services keep serving the raw node ports. Guarded traffic is routed through the group/global Services (whose selectors are flipped to the guard once it is ready) and through the dedicated `<name>-cg` Service.

### Shared cache (olric cluster)

CosmoGuard runs as a StatefulSet so every replica joins one **embedded olric cache cluster** and shares a single distributed cache. A response cached by any replica is served from cache by all of them, so the backing nodes are shielded no matter how the load balancer spreads requests — this is the whole point of running multiple replicas in front of a group. Cosmopilot wires this automatically:

- a **headless peer Service** (`<name>-cg-peer`) gives each replica stable DNS for olric's peer discovery;
- gossip traffic is encrypted with a key Cosmopilot generates once into a **Secret** (`<name>-cg-cluster`) and mounts into every replica.

You don't configure any of this — enabling CosmoGuard is enough.

:::info[Migrating from the sidecar model]
Earlier releases ran CosmoGuard as a sidecar container inside each node pod. Enabling CosmoGuard no longer modifies the node pod. When you upgrade, Cosmopilot brings the standalone guard up first and only routes traffic through it once it is ready (make-before-break), then recreates the node pods without the sidecar. Your rules `ConfigMap` is never modified.
:::

## Traffic While the Guard Starts

Cosmopilot keeps public API routes (Ingress, Gateway routes and the group/global Services) pointed at the node itself until the guard is serving, then switches them to the guard. This is deliberate: CosmoGuard is usually added to nodes that are already serving traffic, and the endpoints must keep working while the guard is being deployed.

The consequence is that **until the guard serves for the first time, public API traffic reaches the node directly and is not filtered** by your rules. That also applies to a node that is created with CosmoGuard enabled, and for as long as the guard cannot start (for example a rules `ConfigMap` that does not exist, an image pull failure or a crash loop). Once routes have switched to the guard they stay there, so a later guard outage makes the endpoints unavailable rather than unfiltered.

Cosmopilot reports this state through the `CosmoGuardReady` condition: on the `ChainNode` for a standalone node, or for a `ChainNodeSet` child with its own individual ingress/gateway (which gets its own guard), and on the `ChainNodeSet` for group guards and the global ingress/gateway routes in front of them. A Gateway route counts as switched only once its parent Gateway has accepted the change. A global route switches to the guard only once every guard replica is up, so it can stay unfiltered for a while after the group guard starts serving; the condition lists it separately. Cosmopilot records a Warning event when the condition turns `False` for a new reason and a Normal event when it recovers:

| Status | Reason | Meaning |
| --- | --- | --- |
| `True` | `CosmoGuardServing` | Every guard is serving and public API routes go through it. |
| `False` | `CosmoGuardNotServing` | A guard is not filtering yet, or a public route has not switched to it. The message names each one and says whether its traffic still reaches the nodes directly (not filtered) or already stays on the guard. |
| `False` | `CosmoGuardConfigMissing` | CosmoGuard is enabled without naming a rules `ConfigMap` (only possible when CRD validation is bypassed), so its guard is not reconciled and traffic may not be filtered. |
| `False` | `CosmoGuardBypassed` | A global route also spans a group without CosmoGuard, so it always reaches the nodes directly and is never filtered. |

```bash
kubectl get chainnodeset <name> -o jsonpath='{.status.conditions[?(@.type=="CosmoGuardReady")]}'
```

If you need the rules enforced from the very first request, wait for `CosmoGuardReady=True` before exposing the endpoints publicly.

Routes configured with `useInternalServices: true` bypass CosmoGuard by design and are not covered by the condition. The condition is refreshed after Cosmopilot reconciles the routes, so it describes the routes as they are; while a reconcile is held before that step (for example a stopped node or a pending signer migration) it keeps its last value.

:::note
`.status.conditions` on `ChainNodeSet` is new. Helm does not upgrade CRDs, so apply the CRDs of the target release before upgrading the operator; otherwise the API server drops the condition and the operator rewrites it on every reconcile (with a Warning event whenever it is `False`).
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
    image: ghcr.io/voluzi/cosmoguard:4.0.3  # Optional: override the operator-wide default image.
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

The image resolves from the per-resource `config.cosmoGuard.image` first, then a nonempty
operator-wide `cosmoGuardImage` Helm value, and finally the pinned default supplied by the selected
manager release. Leave both overrides empty to follow the manager release on upgrades.
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

CosmoGuard ships a read-only web dashboard. Enable it and optionally expose it through either an
Ingress or Gateway API HTTPRoutes (opt-in, off by default):

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

For Gateway API, select the HTTPS listener for the dashboard route and optionally a separate HTTP
listener for an HTTP-to-HTTPS redirect:

```yaml
config:
  cosmoGuard:
    enable: true
    config:
      name: cosmoguard-config
      key: cosmoguard.yaml
    dashboard:
      enable: true
      gateway:
        host: cosmoguard.example.com
        gateway:
          name: external
          namespace: gateway-system     # Optional: defaults to the node's namespace.
          sectionName: https-dashboard
        httpRedirect:                   # Optional: creates a 301 redirect to HTTPS.
          name: external
          namespace: gateway-system
          sectionName: http
```

`ingress` and `gateway` are mutually exclusive. When `httpRedirect` is configured, both parent
references must set `sectionName` and select different listeners. Cosmopilot owns the dashboard
route resources and performs Ingress/Gateway changes make-before-break; if Gateway API CRDs are not
installed, it preserves an existing dashboard Ingress instead of removing the working exposure.

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
