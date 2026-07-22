# Persistence and Backups

This section explains how `Cosmopilot` handles persistence and backups for blockchain node data, ensuring data integrity and seamless recovery.

## Configuration Overview

Persistence settings can be configured in the following locations:
- For `ChainNode`: Configure under `.spec.persistence`.
- For `ChainNodeSet`: 
  - Configure persistence for specific groups under `.spec.nodes[].persistence`.
  - For the validator node (if present), configure under `.spec.validator.persistence`.

For example:
- **ChainNode**:
```yaml
  spec:
    persistence: {...}
```
- **ChainNodeSet**:
```yaml
spec:
  nodes:
    - name: fullnodes
      persistence: {...}
  validator:
    persistence: {...}
```

## PVC Size and Storage Class

Each node deployed by `Cosmopilot` requires a `PVC` for storing its data.

### Default PVC Size
By default, the `PVC` size is set to `50Gi`, with [auto-resize](#auto-resize) feature enabled. This can be changed in [persistence](../reference/crds#persistence) spec:

```yaml
persistence:
  size: 100Gi
```

### Configuring Storage Class
By default, the `PVC` uses the default storage class configured in your Kubernetes cluster. You can specify a custom storage class in the node configuration:

```yaml
persistence:
  storageClass: custom-storage-class
```
 
## Auto-Resize

`Cosmopilot` includes an auto-resize feature that monitors PVC usage and increases the volume size when a configured threshold is exceeded. This feature is enabled by default.

You can configure auto-resize settings in the [persistence](../reference/crds#persistence) configuration. Key options include:
- The **threshold** at which a resize event occurs.
- The **increment** added to the PVC size during each resize.
- The **maximum size** the PVC can reach.

### Example Configuration
```yaml
persistence:
  autoResize: true
  autoResizeThreshold: 70 # Default is 80%
  autoResizeIncrement: 20Gi # Default is 50Gi
  autoResizeMaxSize: 5Ti # Defaults to 2Ti
```


## Additional Volumes

Some applications need to persist data outside the main `data` directory. While it's advisable to configure the application to store additional data within `/home/app/data` using [TOML config overrides](../usage/node-config#overriding-toml-config-files) when possible, this isn't always feasible.

For these cases, you can create additional volumes:

```yaml
persistence:
  additionalVolumes:
    - name: wasm
      size: 1Gi
      path: /home/app/wasm
      deleteWithNode: true
    - name: ibc-08-wasm
      size: 1Gi
      path: /home/app/ibc_08-wasm
      deleteWithNode: true
```

### Configuration Options

| Field | Description |
|-------|-------------|
| `name` | Name of the volume (used for PVC naming) |
| `size` | Size of the volume |
| `path` | Mount path inside the container |
| `storageClass` | Optional. Storage class to use. Defaults to `.persistence.storageClass`, then cluster default |
| `deleteWithNode` | Optional. Whether to delete the PVC when the node is deleted. Defaults to `false` |

### Mounted During Initialization

Additional volumes are also mounted during data initialization, allowing them to be used with `.persistence.additionalInitCommands` to extract snapshots or initialize data directly into these volumes.

## Snapshots

`Cosmopilot` allows configuring periodic snapshots of volumes in order to back up node data. It is also possible to configure cleanup of old snapshots using either time-based retention or count-based retention (by default snapshots are kept forever).

:::warning[NOTE]
When configured on `ChainNodeSet`group, `snapshots` are only taken on one of the nodes of the group (the first node).
:::

### Example configuration

```yaml
persistence:
  snapshots:
    frequency: 24h # Take a snapshot every 24 hours
    retention: 72h # Retain snapshots of the last 3 days
```

### Retention Options

You can configure snapshot retention in two ways:

#### Time-based retention (`retention`)
Delete snapshots after a specified duration:

```yaml
persistence:
  snapshots:
    frequency: 24h
    retention: 72h # Delete snapshots older than 3 days
```

#### Count-based retention (`retain`)
Keep only the N most recent snapshots:

```yaml
persistence:
  snapshots:
    frequency: 24h
    retain: 5 # Keep only the 5 most recent snapshots
```

:::warning[NOTE]
The `retention` and `retain` fields are mutually exclusive. You can only use one of them at a time.
:::

### Snapshot Class

By default, `Cosmopilot` uses the default `VolumeSnapshotClass` configured in your cluster. You can also set a custom one:

```yaml {5}
persistence:
  snapshots:
    frequency: 24h # Take a snapshot every 24 hours
    retention: 72h # Retain snapshots of the last 3 days
    snapshotClass: my-custom-snapshot-class
```

### Stopping the Node for Snapshot

By default, `Cosmopilot` does not stop the node while taking a snapshot. This approach leverages the crash-consistent snapshot capabilities provided by most major cloud providers (e.g., GKE, AWS EKS, Azure AKS), which are generally reliable for blockchain nodes.

However, in environments where crash consistency cannot be guaranteed or when additional safety is required, you can configure `Cosmopilot` to stop the node during the snapshot process. This ensures the node is quiesced and no writes are occurring while the snapshot is taken.

You can enable this behavior with:

```yaml {3}
persistence:
  snapshots:
    stopNode: true
```

:::info[NOTE]
If you prefer to avoid downtime you can consider enabling [integrity verification](#integrity-checks) instead.
:::

#### When to Use
- **Unverified Providers**: If your cluster uses a storage backend without clear guarantees of crash consistency.
- **Heavy Write Workloads**: For nodes experiencing intensive writes, where the risk of snapshot inconsistency is higher.
- **Critical Data**: For validators or nodes with high availability requirements where snapshot integrity is paramount.

### Disable Snapshots While Node Is Syncing

In some cases, it may be beneficial to disable snapshots while the node is syncing blocks (still catching up to the network). This avoids creating snapshots of an outdated or incomplete state.

#### Configuration

To disable snapshots during sync, set the `.persistence.snapshots.disableWhileSyncing` option to `true`:

```yaml
persistence:
  snapshots:
    disableWhileSyncing: true
```

## Integrity Checks

`Cosmopilot` offers the ability to verify the integrity of snapshots by attempting to start a separate node from the snapshot data. This ensures that the snapshot is valid and can be successfully used to restore a node. Integrity checks add an additional layer of safety, especially when snapshots are taken without stopping the node.

### Configuration

To enable integrity checks, set the `.persistence.snapshots.verify` option to `true`:

```yaml
persistence:
  snapshots:
    verify: true
```

### How It Works
1. After a snapshot is created, `Cosmopilot` will:
   - Provision a temporary `PVC` from the snapshot.
   - Launch a test node using the `PVC`.
   - Monitor the test node to confirm it starts and runs successfully.
2. If the test node starts and syncs correctly, the snapshot is marked as valid.
3. If the integrity check fails, the snapshot is deleted, and `Cosmopilot` attempts to take a new snapshot.

## Exporting Tarball

`Cosmopilot` can stream data from a volume snapshot as a tar archive to Google
Cloud Storage (GCS), Amazon S3, or an S3-compatible object store such as MinIO or
DigitalOcean Spaces. Archives can be uncompressed or use gzip, zstd, or lz4.

To enable this functionality, configure the `.persistence.snapshots.exportTarball` field.

### Configuration

Here is a GCS example using zstd compression:

```yaml
persistence:
  snapshots:
    exportTarball:
      suffix: archive
      deleteOnExpire: false
      compression: zstd
      gcs:
        bucket: my-backup-bucket
        credentialsSecret:
          name: gcs-credentials
          key: credentials.json
```

### General Fields
- **`suffix`**: 
  - Optional. Adds a suffix to the tarball name. 
  - The tarball name is `<chain-id>-<timestamp>-<suffix>` when a suffix is set.
  - **Use Cases**:
    - Add context about the tarball (for example, `archive` or `pruned`).
    - Specify the database backend (for example, `goleveldb` or `pebbledb`).
- **`deleteOnExpire`**:
  - Optional. Defaults to `false`.
  - Indicates whether the tarball should also be deleted when the associated volume snapshot is removed due to expiration (`.persistence.snapshots.retention`).
- **`compression`**:
  - Optional. One of `none`, `gzip`, `zstd`, or `lz4`. Defaults to `gzip` for
    compatibility with existing exports.
  - `zstd` is recommended for the best balance of export speed, archive size, and
    restoration speed. `lz4` prioritizes restoration speed, while `gzip` offers
    the broadest compatibility.

The resulting extensions are `.tar`, `.tar.gz`, `.tar.zst`, and `.tar.lz4`.

Exactly one provider, `gcs` or `s3`, must be configured.

### Google Cloud Storage

The following fields are available for GCS:
- **`bucket`**:
  - The name of the `GCS` bucket where the tarball will be uploaded.
- **`credentialsSecret`**:
  - A Kubernetes secret containing the JSON credentials with permissions to upload and delete objects in the specified bucket.
- **`serviceAccountName`**:
  - The name of a Kubernetes `ServiceAccount` that the snapshot upload/delete Jobs run as, so they authenticate to `GCS` through [Workload Identity](#authenticating-with-workload-identity) / Application Default Credentials (ADC) instead of a credentials secret.

:::warning[NOTE]
Exactly one of `credentialsSecret` or `serviceAccountName` must be set. Setting both — or neither — is rejected by the admission webhook.
:::

### Authenticating with Workload Identity

On `GKE`, you can avoid managing a long-lived JSON key entirely by using [Workload Identity](https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity). Instead of a `credentialsSecret`, you reference a Kubernetes `ServiceAccount` that is bound to a Google Service Account (`GSA`) with permissions on the bucket. The snapshot Jobs then obtain credentials automatically via Application Default Credentials.

#### GKE Example

1. Grant the Google Service Account permissions on the bucket (for example `roles/storage.objectAdmin`, which allows both uploading and deleting objects):

```bash
gcloud storage buckets add-iam-policy-binding gs://my-backup-bucket \
  --member="serviceAccount:gcs-uploader@my-project.iam.gserviceaccount.com" \
  --role="roles/storage.objectAdmin"
```

2. Create a Kubernetes `ServiceAccount` in the node's namespace and annotate it with the `GSA` it should impersonate:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: gcs-uploader
  namespace: my-namespace
  annotations:
    iam.gke.io/gcp-service-account: gcs-uploader@my-project.iam.gserviceaccount.com
```

3. Allow the Kubernetes `ServiceAccount` to impersonate the `GSA`:

```bash
gcloud iam service-accounts add-iam-policy-binding \
  gcs-uploader@my-project.iam.gserviceaccount.com \
  --role="roles/iam.workloadIdentityUser" \
  --member="serviceAccount:my-project.svc.id.goog[my-namespace/gcs-uploader]"
```

4. Reference the `ServiceAccount` in the export config, without a `credentialsSecret`:

```yaml
persistence:
  snapshots:
    exportTarball:
      gcs:
        bucket: my-backup-bucket
        serviceAccountName: gcs-uploader
```

### Amazon S3

The S3 exporter uses the AWS SDK default credential chain. It supports static
access keys, IRSA/web identity, EKS Pod Identity, EC2 instance roles, and shared
AWS configuration when mounted into the Job.

```yaml
persistence:
  snapshots:
    exportTarball:
      compression: zstd
      deleteOnExpire: true
      s3:
        bucket: cosmos-snapshots
        region: eu-west-1
        serviceAccountName: snapshot-exporter
```

The IAM identity needs `s3:PutObject`, `s3:AbortMultipartUpload`,
`s3:ListBucket`, and `s3:DeleteObject` for the configured bucket and prefix.

S3 uploads use multipart requests with a default `chunkSize` of `64MB`. Amazon
S3 allows at most 10,000 chunks per object, so the exporter can split an archive
before `sizeLimit` when necessary. The default `partSize` is `500GB`; increase
`chunkSize` or lower `partSize` if a custom combination cannot fit within the
multipart limit.

#### IRSA and EKS Pod Identity

Set `serviceAccountName` to a Kubernetes ServiceAccount configured for IRSA or
EKS Pod Identity. The EKS integration injects the web identity or container
credentials consumed by the default AWS credential chain.

#### Access keys

Create a Secret using standard AWS environment variable names:

```bash
kubectl create secret generic s3-credentials \
  --from-literal=AWS_ACCESS_KEY_ID='<access-key-id>' \
  --from-literal=AWS_SECRET_ACCESS_KEY='<secret-access-key>'
```

Reference it from the export configuration:

```yaml
persistence:
  snapshots:
    exportTarball:
      compression: lz4
      s3:
        bucket: cosmos-snapshots
        region: eu-west-1
        credentialsSecret:
          name: s3-credentials
```

`AWS_SESSION_TOKEN` can be added to the same Secret for temporary credentials.
`credentialsSecret` and `serviceAccountName` are mutually exclusive. When both
are omitted, the exporter relies entirely on the default credential chain, which
supports EKS Pod Identity and EC2 instance roles.

#### S3-compatible storage

Set `endpoint` for the provider API. Enable `forcePathStyle` when the provider
does not support virtual-hosted bucket names, as is common with MinIO:

```yaml
persistence:
  snapshots:
    exportTarball:
      compression: zstd
      s3:
        bucket: cosmos-snapshots
        region: us-east-1
        endpoint: http://minio.storage.svc.cluster.local:9000
        forcePathStyle: true
        credentialsSecret:
          name: minio-credentials
```

For DigitalOcean Spaces, use the region-specific HTTPS endpoint and normally
leave `forcePathStyle` disabled.

### Restoring exported archives

After downloading an archive, extract it into the node home directory using the
command matching its extension:

```bash
tar -xf snapshot.tar
tar -xzf snapshot.tar.gz
zstd -dc snapshot.tar.zst | tar -xf -
lz4 -dc snapshot.tar.lz4 | tar -xf -
```

Archives exceeding `sizeLimit` are stored as ordered parts. Concatenate them
before decompression, for example:

```bash
cat snapshot-part-*.tar.zst | zstd -dc | tar -xf -
```


## Restoring Data from Snapshot

For detailed instructions on restoring data from a snapshot, refer to the [Restore from Snapshot](../usage/restoring-from-snapshot) page.
