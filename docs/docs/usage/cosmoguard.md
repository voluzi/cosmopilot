# Using CosmoGuard

[CosmoGuard](https://github.com/voluzi/cosmoguard) is a lightweight firewall designed specifically for protecting Cosmos nodes. With CosmoGuard you can control access at the API endpoint level, cache responses for performance, rate-limit clients, and limit WebSocket connections for better resource management.

`Cosmopilot` integrates with CosmoGuard **v6.1.0** and deploys it as a **standalone clustered StatefulSet** that sits in front of your node(s), rather than as a sidecar container inside the node pod.

## Upgrading to CosmoGuard 6.1.0

Upgrading from operator v5.1.0 changes the default guard image from 5.0.0 to 6.1.0
and the per-replica resources from 200m CPU / 250Mi memory to **500m CPU / 500Mi
memory, with requests equal to limits**. Defaulted guard StatefulSets roll automatically
as they reconcile; no rules-file change is required. The new soft hostname spread
constraint changes every managed guard pod template, including guards with image
and resource overrides, so every guard StatefulSet rolls once on operator
upgrade. Adopting the new module's
configuration baseline does not cause an additional restart.

Plan a low-traffic window. Upstream observed roughly a minute of reduced throughput
per guard cluster during the first v5.x-to-v6 rollout, with three replicas at
200m/250Mi and zero container restarts. Severe dips lasted approximately 45–90
seconds, within a longer rollout and partial-capacity period. These measurements
are not a guaranteed duration or a promise of zero errors. A StatefulSet pod
replacement is distinct from a container restart within a pod. A single-replica
guard is unavailable until its replacement is ready; multiple replicas reduce
disruption but do not eliminate mixed-v5/v6 degradation.

Reserve an additional 300m CPU and 250Mi memory per defaulted replica: three
replicas request 1.5 CPU and 1500Mi. Complete explicit resources and the existing
image override precedence remain unchanged. With HPA enabled, empty or incomplete
resource overrides can still receive default requests for the selected metrics;
higher CPU requests can change percentage-based scaling. Capacity and inherited
placement constraints can leave replacements Pending.

V6 automatically derives its bounded L2 storage budgets from the container memory
limit; no new configuration is required. See the upstream
[v6 upgrade guide](https://github.com/voluzi/cosmoguard/blob/v6.1.0/docs/upgrade-v6.md)
for measured rollout results, malformed HTTP query sanitization and verification
limits, and [configuration reference](https://github.com/voluzi/cosmoguard/blob/v6.1.0/CONFIG.md#memory-budget)
for memory budgets.

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

### Host spreading

Every guard has a soft topology spread preference over `kubernetes.io/hostname`,
with `maxSkew: 1` and `ScheduleAnyway`. Its selector counts only that guard's
replicas. This applies to a single replica, fixed multiple replicas and HPA-managed
guards, including an HPA starting at one replica. It prefers different eligible
hosts while allowing colocation on a one-host pool or when capacity or inherited
placement limits choices. Existing node selectors and affinity remain in force.
This does not guarantee one replica per host or rebalance already-running pods;
the preference takes effect when replacements are scheduled.

### Shared cache (olric cluster)

CosmoGuard runs as a StatefulSet so every replica joins one **embedded olric cache cluster** and shares a single distributed cache. A response cached by any replica is served from cache by all of them, so the backing nodes are shielded no matter how the load balancer spreads requests — this is the whole point of running multiple replicas in front of a group. Cosmopilot wires this automatically:

- a **headless peer Service** (`<name>-cg-peer`) gives each replica stable DNS for olric's peer discovery;
- gossip traffic is encrypted with a key Cosmopilot generates once into a **Secret** (`<name>-cg-cluster`) and mounts into every replica.

The per-guard `<name>-cg-cluster` Secret holds the gossip encryption key; if it is deleted, Cosmopilot recreates it with a new key while running replicas retain the old one and cannot form a cluster with replicas started later, so restart the guards after deletion.

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

## Configuration updates

Changes to rules, trusted proxies, request logging, JSON-RPC batch size and gRPC protosets hot-reload without restarting guard or node pods. When a file change requires a restart according to CosmoGuard's own policy (for example, authentication, cache topology or server timeouts), Cosmopilot requests the guard StatefulSet's ordered rolling update; with two or more replicas, old and new replicas can serve together until it completes. Classification uses the CosmoGuard module bundled with the operator, so an overridden guard image may have a different reload policy.

An operator upgrade adopts the current valid configuration without restarting existing guards solely because the rollout baseline is introduced, including when the bundled CosmoGuard module version changes; a guard image change still triggers its normal StatefulSet rollout. Invalid files leave the rollout state unchanged and do not block other reconciliation; CosmoGuard rejects and logs them itself. The `cosmopilot.voluzi.com/cosmoguard-*` annotations are operator-managed rollout state, with digests protected by the existing per-guard cluster encryption key.

`${VAR}` references in the rules file are resolved by Cosmopilot only against the variables it renders into the guard pod, so a file requiring any other variable (for example `${HOSTNAME}`) cannot be classified and its restart-required changes are not rolled out automatically.

With a single replica (the default), the guard is unavailable for the duration of a pod restart caused by a restart-required change.

Dashboard credentials are read from Secrets at startup, so rotating those Secrets rolls the guards on the next reconcile of the owning ChainNode or ChainNodeSet; referenced credential Secrets are not watched.

## Probes and termination

The startup probe uses `/healthz` on port 9001 every two seconds with a failure
threshold of 30. V6 answers health during bootstrap, so this checks the process
and operations listener rather than imposing a 60-second cluster-join deadline.
Readiness uses `/readyz` every five seconds with a failure threshold of three,
keeping a joining guard out of traffic until it can serve. The binary caps total
bootstrap waiting at ten minutes and can fail earlier on discovery or
coordinator reachability. Liveness uses `/healthz` every ten seconds with a
failure threshold of three; a healthy bootstrap wait does not trigger a restart.

The pod explicitly has a 30-second termination grace period, matching Kubernetes'
default. At SIGTERM, v6 fails readiness, continues serving for five seconds, then
drains and cleans up within an absolute 29-second budget. No preStop hook is
needed. WebSocket clients must reconnect and resubscribe after replacement.

## Applying the local testnet example

The [Nibiru guard example](../examples/nibiru/testnet-cosmoguard.md) includes its rules
ConfigMap and initialises a local chain. It uses three fixed guard replicas and the operator's
image and resource defaults. The dashboard is internal and has no basic authentication:

```bash
kubectl port-forward svc/nibiru-testnet-fullnodes-cg 8080:8080
```

Open `http://localhost:8080`. The commented autoscaling alternative requires metrics-server;
no Ingress controller is required by this example.

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

Since v5, CosmoGuard **validates rules strictly**: unknown keys are rejected, every rule and section `default` must use `action: allow` or `action: deny`, and every `rateLimit` block needs a positive `rate`. A rules file that CosmoGuard v4 accepted can fail startup on later versions, so validate it before upgrading.

In v4, CosmoGuard **removed Redis**: a `cache.backend`, `cache.redis` or `cache.redis-sentinel` key now fails startup. For multi-replica caches CosmoGuard uses an embedded cluster; single-replica needs no cache backend at all. See the CosmoGuard [v4 migration notes](https://github.com/voluzi/cosmoguard/blob/main/CONFIG.md) for other breaking changes (WebSocket cross-origin now denied by default, CosmoGuard owns CORS, gRPC reflection is no longer auto-allowed). You can validate a file with `cosmoguard validate --config <file>`.
:::

:::warning[WebSocket connections behind an Ingress or Gateway]
CosmoGuard limits WebSocket connections per client IP (`server.websocketLimits.maxConnectionsPerIP`, default 16). When CosmoGuard is exposed through an Ingress or Gateway, it sees the proxy's pod IP unless that proxy is listed in `server.trustedProxies`, so every client behind one proxy pod shares those 16 connections. Add your Ingress or Gateway proxy addresses to `server.trustedProxies` in the rules file (CosmoGuard then uses the client address from `X-Forwarded-For`), or raise the limit (`0` disables it).
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
    image: ghcr.io/voluzi/cosmoguard:6.1.0  # Optional: override the operator-wide default image.
    resources:                 # Optional: per-pod resources (defaults shown).
      requests:
        cpu: 500m
        memory: 500Mi
      limits:
        cpu: 500m
        memory: 500Mi
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

- **Match expressively:** CosmoGuard supports an expressive `match` tree (`all`/`any`/`none` + `path`/`method`/`query`/`header`/`sourceIP`) with glob values.
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
