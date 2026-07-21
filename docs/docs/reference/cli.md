# CLI & Environment Variables

`Cosmopilot` ships several binaries. In normal operation you only interact with the
**manager** (through its Helm values) — the other binaries run automatically inside
the Pods the operator creates. This page documents every command, flag and
environment variable for completeness and for advanced/debugging scenarios.

:::tip
Most users never set these directly. The Helm chart maps the relevant manager
settings to friendly values — see [Configuration](../getting-started/configuration).
:::

Each flag has an equivalent environment variable. When both are set, the **flag wins**.

## manager

The operator process. It runs the `ChainNode` and `ChainNodeSet` controllers and the
admission webhook server.

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `-metrics-bind-address` | `METRICS_BIND_ADDRESS` | `:8080` | Address the metrics endpoint binds to. |
| `-health-probe-bind-address` | `HEALTH_PROBE_BIND_ADDRESS` | `:8081` | Address the health/readiness probe endpoint binds to. |
| `-enable-leader-election` | `ENABLE_LEADER_ELECTION` | `false` | Enable leader election so only one manager is active at a time. |
| `-nodeutils-image` | `NODE_UTILS_IMAGE` | `ghcr.io/voluzi/node-utils` | `node-utils` image deployed as a sidecar with each node. |
| `-cosmoguard-image` | `COSMOGUARD_IMAGE` | `ghcr.io/voluzi/cosmoguard:4.0.0-rc.7` | CosmoGuard image for the standalone deployments created when CosmoGuard is enabled. |
| `-cosmoseed-image` | `COSMOSEED_IMAGE` | `ghcr.io/voluzi/cosmoseed` | Image used for Cosmoseed deployments when enabled. |
| `-worker-name` | `WORKER_NAME` | `""` | Name of this worker (set as the `worker-name` label). Used to shard which resources this instance reconciles. |
| `-worker-count` | `WORKER_COUNT` | `1` | Maximum number of concurrent reconciles. |
| `-disable-webhooks` | `DISABLE_WEBHOOKS` | `false` | Disable admission webhooks. |
| `-debug-mode` | `DEBUG_MODE` | `false` | Enable verbose, development-style logging. |
| `-certs-dir` | `CERTS_DIR` | `""` | Directory where the manager looks for webhook serving certificates. |
| `-release-name` | `RELEASE_NAME` | `cosmopilot` | Helm release name; used to resolve the PriorityClass names assigned to Pods. |
| `-disruption-checks-enabled` | `DISRUPTION_CHECKS_ENABLED` | `true` | Enable Pod disruption checks. |
| `-disruption-max-unavailable` | `DISRUPTION_MAX_UNAVAILABLE` | `1` | Maximum number of unavailable Pods sharing the same labels. |

Fixed (not configurable) endpoints:

- **Webhook server**: port `9443`.
- **Leader election ID**: `<release-name>.cosmopilot.voluzi.com`, or
  `<worker-name>.<release-name>.cosmopilot.voluzi.com` when `worker-name` is set.

:::note
When installed via Helm, the chart sets `ENABLE_LEADER_ELECTION=true` and maps
`workerCount` (default `10` in the chart), `workerName`, `webHooksEnabled`,
`debugMode` and `disruptionChecksEnabled` to the corresponding variables.
:::

## node-utils

The helper sidecar that runs in every node Pod and exposes an internal HTTP API
(default port `8000`) used by the operator. You generally never run this yourself.

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `-host` | `HOST` | `0.0.0.0` | Host the server listens on. |
| `-port` | `PORT` | `8000` | Port the server listens on. |
| `-data-dir` | `DATA_DIR` | `/home/app/data` | Directory where the data volume is mounted. |
| `-block-threshold` | `BLOCK_THRESHOLD` | `0` (disabled) | Time to wait for a new block before the node is considered unhealthy. |
| `-upgrades-config` | `UPGRADES_CONFIG` | `/config/upgrades.json` | File containing the upgrades configuration. |
| `-trace-store` | `TRACE_STORE` | `/trace/trace.fifo` | File or FIFO watched for traces. |
| `-log-level` | `LOG_LEVEL` | `info` | Log level. |
| `-create-fifo` | `CREATE_FIFO` | `false` | Create the FIFO for the trace store. |
| `-tmkms-proxy` | `TMKMS_PROXY` | `false` | Enable the TMKMS proxy. |
| `-node-binary-name` | `NODE_BINARY_NAME` | `""` | Name of the node application binary. |
| `-halt-height` | `HALT_HEIGHT` | `0` (disabled) | Height at which the node will be halted. |
| `-mock-mode` | `MOCK_MODE` | `false` | Enable mock mode (returns configurable stats instead of real process stats). For E2E testing only. |

### `node-utils mock`

Helper subcommands used by E2E tests to drive a sidecar running in mock mode
(via `kubectl exec`). They talk to the local server on `PORT` (default `8000`).

```bash
node-utils mock set-cpu <millicores>   # e.g. 500 for 500m
node-utils mock set-memory <mib>       # e.g. 512 for 512 MiB
node-utils mock get                    # print current mock stats
```

## dataexporter

CLI tool for uploading and deleting snapshot tarballs in external storage. The
operator invokes it automatically when exporting snapshots; the reference below is
for manual or debugging use.

```bash
dataexporter gcs upload <dir> <bucket> <name>
dataexporter gcs delete <bucket> <name>
```

Persistent flag (all subcommands):

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `--log-level` | `LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error`, `fatal`, `panic`. |

### `gcs upload`

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `--chunk-size` | `CHUNK_SIZE` | `250MB` | Chunk size for multi-part uploads. |
| `--part-size` | `PART_SIZE` | `500GB` | Part size for multi-part uploads (used when the size limit is crossed). |
| `--size-limit` | `SIZE_LIMIT` | `5TB` | Size limit for a single file. |
| `--report-period` | `REPORT_PERIOD` | `1s` | How often upload progress is reported. |
| `--concurrent-jobs` | `CONCURRENT_JOBS` | `10` | Number of concurrent upload jobs. |
| `--buffer-size` | `BUFFER_SIZE` | `32MB` | Upload buffer size. |

### `gcs delete`

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `--concurrent-jobs` | `CONCURRENT_JOBS` | `10` | Number of concurrent delete jobs. |

## vault-token-renewer (deprecated)

This deprecated sidecar keeps a HashiCorp Vault token renewed for legacy TMKMS
configurations when `autoRenewToken` is enabled. It remains available during the TMKMS
deprecation period for compatibility and should not be used by new deployments. Migrate to
[Cosmosigner](../usage/cosmosigner), which manages Vault token renewal internally. You do not run
or configure the sidecar manually.
