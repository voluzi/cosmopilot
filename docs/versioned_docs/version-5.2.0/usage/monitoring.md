# Monitoring & Observability

`Cosmopilot` exposes metrics and health information at two levels: the **nodes** it
manages, and the **operator** itself. This page explains what is available and how to
scrape it.

## Node metrics

Every node `Cosmopilot` deploys has CometBFT/Cosmos SDK Prometheus instrumentation
enabled by default (`instrumentation.prometheus = true` in `config.toml`). Metrics are
served on the `prometheus` port:

| Port name | Port |
| --- | --- |
| `prometheus` | `26660` |

This port is exposed both on the node Pod and on the node `Service` (named after the
`ChainNode`), so it can be scraped like any other Kubernetes target. The metrics are
the standard CometBFT (`cometbft_*`) and Cosmos SDK metrics — block height, peer
counts, consensus timings, mempool size, and so on.

:::tip
`Cosmopilot` itself uses these metrics internally — for example, it reads CometBFT
state-sync chunk metrics to decide when a state-syncing node has finished.
:::

### Scraping nodes with the Prometheus Operator

`Cosmopilot` does **not** create `ServiceMonitor` resources for you, so you keep full
control over what gets scraped. To scrape nodes with the
[Prometheus Operator](https://github.com/prometheus-operator/prometheus-operator),
add a label to the `ChainNode`/`ChainNodeSet` (labels propagate to the node `Service`)
and select on it.

For example, label your nodes:

```yaml
metadata:
  labels:
    monitoring: enabled
```

Then create a `ServiceMonitor` that targets the `prometheus` port:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: cosmopilot-nodes
  namespace: monitoring
spec:
  namespaceSelector:
    any: true
  selector:
    matchLabels:
      monitoring: enabled
  endpoints:
    - port: prometheus
      interval: 30s
```

If you don't run the Prometheus Operator, any plain Prometheus scrape config that
discovers the node Services and scrapes port `26660` works just as well.

## Operator metrics

The manager serves its own metrics — standard
[controller-runtime](https://book.kubebuilder.io/reference/metrics) metrics such as
reconcile counts, reconcile latency, work-queue depth, and Go runtime metrics — on:

| Address | Default | Description |
| --- | --- | --- |
| `--metrics-bind-address` | `:8080` | Operator (controller-runtime) metrics. |

The Helm chart does not create a dedicated metrics `Service` or `ServiceMonitor` for
the manager, so if you want to scrape operator metrics, expose port `8080` with your
own `Service` and scrape config (or `ServiceMonitor`). See the
[CLI reference](../reference/cli#manager) to change the bind address.

## Health & readiness

The manager exposes liveness and readiness probes:

| Address | Default | Endpoints |
| --- | --- | --- |
| `--health-probe-bind-address` | `:8081` | `/healthz` (liveness), `/readyz` (readiness) |

When installed via Helm with `probesEnabled: true` (the default), these are wired to
the Deployment's liveness and readiness probes automatically.

## node-utils internal API

Each node Pod runs a `node-utils` sidecar that exposes an internal HTTP API on port
`8000`. This API is consumed by the operator (for data size, latest height, upgrade
detection, graceful shutdown, etc.) and is **not** a Prometheus endpoint — treat it as
internal. For node observability, scrape the `prometheus` port described above.

## What to alert on

A few practical starting points for alerts, using node metrics:

- **Block height not advancing** — the node is stuck or syncing.
- **Low or zero connected peers** — networking/peering problems.
- **Missed validator signatures** (for validators) — risk of jailing.
- **Disk usage approaching capacity** — although `Cosmopilot` auto-resizes PVCs, alert
  in case auto-resize is disabled or the storage class can't expand.

See [Troubleshooting](../operations/troubleshooting) for how to react to these.
