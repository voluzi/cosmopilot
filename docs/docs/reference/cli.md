# CLI & Environment Variables

`Cosmopilot` ships several binaries. In normal operation you only interact with the
**manager** (through its Helm values) — the other binaries run automatically inside
the Pods the operator creates. This page documents every command, flag and
environment variable for completeness and for advanced/debugging scenarios.

:::tip
Most users never set these directly. The Helm chart maps the relevant manager
settings to friendly values — see [Configuration](../getting-started/configuration).
:::

Where listed, environment variables provide flag defaults. When both are set, the **flag wins**.

## manager

The operator process. It runs the `ChainNode` and `ChainNodeSet` controllers and the
admission webhook server.

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `-metrics-bind-address` | `METRICS_BIND_ADDRESS` | `:8080` | Address the metrics endpoint binds to. |
| `-health-probe-bind-address` | `HEALTH_PROBE_BIND_ADDRESS` | `:8081` | Address the health/readiness probe endpoint binds to. |
| `-enable-leader-election` | `ENABLE_LEADER_ELECTION` | `false` | Enable leader election so only one manager is active at a time. |
| `-nodeutils-image` | `NODE_UTILS_IMAGE` | Pinned manager-release default | `node-utils` image deployed as a sidecar with each node. |
| `-cosmoguard-image` | `COSMOGUARD_IMAGE` | Pinned manager-release default | CosmoGuard image for the standalone StatefulSets created when CosmoGuard is enabled. |
| `-cosmoseed-image` | `COSMOSEED_IMAGE` | Pinned manager-release default | Image used for Cosmoseed deployments when enabled. |
| `-cosmosigner-image` | `COSMOSIGNER_IMAGE` | Pinned manager-release default | Default image for managed Cosmosigner deployments; a per-resource image overrides it. |
| `-dataexporter-image` | `DATA_EXPORTER_IMAGE` | Pinned manager-release default | Data exporter image used by snapshot upload and deletion Jobs. |
| `-utility-image` | `UTILITY_IMAGE` | Pinned manager-release default | Utility image used by operator-owned helper containers. |
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
`debugMode` and `disruptionChecksEnabled` to the corresponding variables. Companion image
variables are emitted only for nonempty Helm values; empty values inherit the pinned defaults from
the selected manager release.
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
| `-log-level` | `LOG_LEVEL` | `info` | Log level. |
| `-node-binary-name` | `NODE_BINARY_NAME` | `""` | Name of the node application binary. |
| `-halt-height` | `HALT_HEIGHT` | `0` (disabled) | Height at which the node will be halted. |
| `-mock-mode` | `MOCK_MODE` | `false` | Enable mock mode (returns configurable stats instead of real process stats). For E2E testing only. |

### `node-utils wait-for-signer`

The operator runs this command as a target node's final init container:

```bash
node-utils wait-for-signer <port> <discovery-hostname> <pod-ip> <timeout>
```

It listens on the privval port until a connection sends at least one byte, closes the connection,
and exits so the app can bind the same port. Bare TCP connections do not release the gate.
The operator supplies port `26659` and timeout `25s`; DNS observations accompany timeout errors.

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
dataexporter s3 upload <dir> <bucket> <name>
dataexporter s3 delete <bucket> <name>
```

Persistent flag (all subcommands):

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `--log-level` | `LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error`, `fatal`, `panic`. |

### `gcs upload`

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `--compression` | `COMPRESSION` | `gzip` | Archive compression: `none`, `gzip`, `zstd`, or `lz4`. |
| `--result-file` | None | Disabled | Write a validated upload result to the given path after successful upload; maximum 3,072 JSON bytes. |
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

### `s3`

The AWS SDK default credential chain supports environment variables, shared AWS
configuration, web identity tokens such as IRSA, EKS Pod Identity, and EC2 instance
roles.

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `--region` | `AWS_REGION` / `AWS_DEFAULT_REGION` | empty | AWS region used to sign requests. |
| `--endpoint` | `S3_ENDPOINT` | empty | Custom S3-compatible endpoint URL. |
| `--force-path-style` | `S3_FORCE_PATH_STYLE` | `false` | Use path-style bucket addressing. |

The `s3 upload` flags match `gcs upload`, except its default `--chunk-size` is
`64MB`. The `s3 delete` command supports `--concurrent-jobs`.


### Upload results

Both upload commands support an explicit result path:

```bash
dataexporter s3 upload /data snapshots chain/attempt-123 --compression zstd --result-file /tmp/upload-result.json
```

Without `--result-file`, uploads retain their existing error-only behavior and create no result
file. With the flag, the command calls the context-aware `UploadWithResult` API and writes exactly
one JSON object after acknowledged completion. An empty path, unsupported result, oversized JSON,
upload failure, or file-write failure makes the command unsuccessful. The limit applies to encoded
UTF-8 JSON, including escaped characters; fields are never truncated. Use a fresh result path for
each invocation and accept it only when the command exits successfully. A file-write failure does
not roll back completed remote output.

Version 1 has the following exact fields:

| JSON key | Type | Contract |
| --- | --- | --- |
| `version` | Integer | Required; `1`. |
| `layout` | String | Required; `single` or `split`. |
| `objectCount` | Integer | Required; positive count of acknowledged final objects, excluding temporary chunks. Must be `1` for `single`. |
| `compression` | String | Required; exactly `none`, `gzip`, `zstd`, or `lz4`. |
| `objectKey` | String | Required and nonempty for `single`; exact completed key, including prefix and archive extension. Absent for `split`. |
| `sha256` | String | Required for `single`; 64 lowercase hexadecimal characters hashing the complete stored bytes. Absent for `split`. |
| `sizeBytes` | Integer | Required and positive for `single`; complete stored byte count after compression. Absent for `split`. |
| `cleanupRequired` | Boolean | Always present; temporary GCS objects could not all be deleted after final output was acknowledged. |

For example, a single-object result has this shape:

```json
{"version":1,"layout":"single","objectCount":1,"compression":"zstd","objectKey":"chain/attempt-123.tar.zst","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sizeBytes":1234,"cleanupRequired":false}
```

A conservative split path can produce just one final object and still reports `split`:

```json
{"version":1,"layout":"split","objectCount":1,"compression":"gzip","cleanupRequired":false}
```

The digest includes tar padding and compression trailers. It is independent of S3 multipart ETags,
GCS checksums, and export identity hashes. Multipart S3 uploads can produce a `single` result only
after successful multipart completion; GCS requires successful final composition or copy. A failed
or lost completion acknowledgement returns no successful result, even if remote output exists.
Split results make no single-object restoration claim, regardless of their actual object count.

The result API and result-file command treat `cleanupRequired: true` as completed output with
remaining cleanup; they do not upload or compose the final object again to retry cleanup. Legacy
GCS uploads retain their error-only cleanup-failure behavior. Results contain no raw provider errors
or credentials.

GCS retains each composition round's temporary inputs until the final output is acknowledged.
Large uploads requiring several rounds therefore need storage for those intermediate objects as
well as the final archive.

This result identifies archive bytes. It does not attest the source snapshot, mounted volume,
worker, Job, Pod, or signing history. It is not a provenance-backed restore descriptor and does not
change the operator's export/controller contract.

### `dataexporter native-upload` and `native-cleanup`

These fixed worker commands implement `exportTarball.protocol: source-v1`. They take
`--namespace`, `--owner`, `--owner-uid`, `--attempt-id`, `--attempt-sequence`, and
`--pod-uid`. The latter defaults to `EXPORT_POD_UID`, supplied by the operator
through the downward API. Diagnostic invocations can set `--pod-uid` explicitly
to identify the already-selected worker; it does not grant authority. `--kubeconfig` is
available for diagnostic execution; workers use the projected Kubernetes token.
Destination, credentials reference, compression and sizing come from the immutable
canonical recipe in ChainNode status. These commands do not accept archive paths
or destination overrides.

An uncached authority GET must authorize the exact selected mounted Pod before an
S3 client is constructed or the archive directory is inspected. Pending authority
may wait for two minutes. Missing, terminal, substituted or unsupported upload
authority fails closed. Secret mode exposes only `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY` and optional `AWS_SESSION_TOKEN`; SDK environment or shared
configuration endpoint overrides are ignored.

The upload writes a separate provenance envelope to `/dev/termination-log`, bounded
to 3,072 UTF-8 bytes. The controller requires a successful exact Job and selected Pod,
strict envelope validation and unchanged source/clone binding before publishing an
archive descriptor. Standalone provider `upload --result-file` keeps its existing
result schema. Split output completes without a single-archive descriptor.

Native cleanup authorizes a selected cleanup Pod against a succeeded attempt and
its deleting catalog entry. Deleting owners and every stopped cleanup state
(`Failed`, `Acknowledged`, `Abandoned`) deny authorization immediately, including
before a cleanup Pod has been selected. It deletes
the acknowledged exact key, or the exact generated keys for acknowledged split
output, with bounded concurrent workers. It never lists by prefix.
