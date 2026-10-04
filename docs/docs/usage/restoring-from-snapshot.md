# Restore from Snapshot

This page explains how to restore blockchain node data using `Cosmopilot`, including state-sync, restoring from volume snapshots, and custom snapshot restore methods.

## Using State-Sync

### From Another Node Managed by Cosmopilot

If there is a node configured to perform state-sync snapshots (as explained in the [Node Configurations](../usage/node-config#state-sync-snapshots) page), it is enough to enable:

```yaml
stateSyncRestore: true
```

`Cosmopilot` will take care of retrieving updated data such as trust height, trust hash, and `RPC` servers, and apply it to this ChainNode.

### From External Nodes

For external nodes, you can manually provide the necessary details by [overriding TOML configuration files](../usage/node-config#overriding-toml-config-files).

Example configuration:

```yaml
config:
  override:
    config.toml:
      statesync:
        enable: true
        rpc_servers: https://rpc.nibiru.fi:443,https://rpc.nibiru.fi:443
        trust_height: 17849562
        trust_hash: A8A55B09347E9BCC6A626D25EDEE2BA063812D2AC335B5EDCDB400239AD8CFE0
```

### Specifying State-Sync Resources

The state sync process may cause the node to consume more resources than during its usual operation. To avoid reconfiguring the node’s resource limits, you can define separate resource specifications that will be applied to the pod while the `ChainNode` is in the `StateSyncing` status.

```yaml {9-15}
resources:
  requests:
    cpu: "500m"
    memory: "1Gi"
  limits:
    cpu: "1"
    memory: "2Gi"
  
stateSyncResources:
  requests:
    cpu: "1000m"
    memory: "2Gi"
  limits:
    cpu: "2"
    memory: "4Gi"
```

:::info[NOTE]
[Vertical pod autoscaling](../usage/vertical-pod-autoscaling) is automatically disabled for a `ChainNode` has the `StateSyncing` status to prevent it from restarting. Once state synchronization is complete, VPA is re-enabled.
:::

## Restoring from a Volume Snapshot

You can obtain the list of available volume snapshots by running

```bash
$ kubectl get volumesnapshots
```

To restore a node from a previously created volume snapshot, use the following configuration:

```yaml
persistence:
  restoreFromSnapshot:
    name: nibiru-testnet-1-fullnode-20241107112229
```

This will instruct `Cosmopilot` to create a Persistent Volume Claim (PVC) from the specified snapshot and attach it to the node.

## Restoring an Exported Snapshot

Set `persistence.restore` to initialize a new node's data volume from one explicitly named,
unsplit object in S3, S3-compatible storage, or GCS. To restore an existing node, set this
configuration and delete its data PVC; Cosmopilot recreates the volume and initializes it from
the configured object. Changing or removing restore configuration does not affect an initialized
volume until it is recreated. PVC deletion discards its current data; Kubernetes PVC protection
may defer deletion until Pods mounting it are stopped and removed.

```yaml
persistence:
  size: 500Gi
  initTimeout: 2h
  restore:
    snapshot:
      provider: s3
      bucket: cosmos-backups
      name: nibiru-1-20260906120000.tar.zst
      region: us-east-1
      endpoint: https://object-storage.example.com
      forcePathStyle: true
      credentialsSecret:
        name: backup-reader
    # Optional: expected SHA-256 of the complete stored object.
    verification:
      sha256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
```

For AWS S3, omit `endpoint` and use the bucket's region. S3 credentials use Secret `envFrom`
with standard AWS environment variables, or the SDK's credential chain. For GCS, use:

```yaml
persistence:
  size: 500Gi
  initTimeout: 2h
  restore:
    snapshot:
      provider: gcs
      bucket: cosmos-backups
      name: nibiru-1-20260906120000.tar.gz
      serviceAccountName: snapshot-reader
      # Alternatively, mount a JSON credential Secret:
      # credentialsSecret:
      #   name: backup-reader
      #   key: credentials.json
```

GCS uses Application Default Credentials with the chosen ServiceAccount, or the selected Secret
key (default `credentials.json`) through `GOOGLE_APPLICATION_CREDENTIALS`. Grant object-read
permission; restoration needs no listing, upload, or deletion permissions. Wrong storage settings,
credentials, images, or volume sizes fail through the normal SDK, Kubernetes, or application path.

Supported formats are `.tar`, `.tar.gz`, `.tar.zst`, and `.tar.lz4`. Supply the exact object key,
including its extension; there is no latest-object selection. Split exports, links, special files,
unsafe paths, and conflicting archive paths are unsupported. Archives contain the data directory,
not the whole application home. Continue configuring a normal genesis source and an application
image compatible with the archived database. Set the initial PVC size for the extracted data and
increase `initTimeout` for large backups; the transfer streams directly into the volume without
storing a compressed copy or resizing storage during initialization.

`verification.sha256` is optional. When supplied, it must match the SHA-256 of the complete stored
object, including compression bytes. Get it from your trusted backup record; the exporter does not
create a checksum manifest, and multipart S3 ETags are not SHA-256 digests. A matching digest
checks bytes, not database consistency, chain identity, application compatibility, or canonical app
state. Export-time `snapshots.verify` remains an application readability check.

Restore replaces the application's data-initialization container in the existing init-data Pod.
Persistence `additionalInitCommands` run afterward. A CSI `restoreFromSnapshot` initializes the
PVC directly and takes precedence; state-sync configuration retains its existing startup behavior.
Use the desired initialization source without combining unrelated sources. The same `restore`
configuration can be set on ChainNodeSet node groups and validator persistence; every instance
initializes independently, regardless of `snapshotNodeIndex`.

The existing `InitializingData` phase and `DataInitStarted`, `DataInitFailed`, and `DataInitialized`
Events describe installation. Failed restores include a download, verification, or extraction
message in the failure Event and container termination message. An initialized volume is never
extracted over. The helper also refuses nonempty targets, leaving partial data untouched after an
interrupted or failed extraction; recreate that PVC to retry. After installation, ordinary node
startup, syncing, and diagnostics apply. There is no automatic compatibility selection, traffic
cutover, health-gated adoption, or rollback.

For validators signing with a local key, the archive carries the signing state from the moment it
was taken: never run the restored node alongside another node using the same key, and do not
restore while the chain is halted at a height that validator already voted on. Managed Cosmosigner
keeps its signing high-water mark in its separate Raft state; restoring node data does not modify
Cosmosigner volumes or state. `priv_validator_state.json` is restored like any other archive file.

## Custom Snapshot Restore

`Cosmopilot` allows you to specify additional commands (containers) to run during the initialization of the data volume. This can be used to, for example, download a tarball and extract it into the data directory.

Example configuration:

```yaml
persistence:
  additionalInitCommands:
  - image: alpine # Optional. Defaults to app image.
    command: ["sh"] # Optional. Defaults to image entrypoint.
    args: ["-c", "wget -qO- https://remote.tarball.here | tar xvf - -C /home/app/data"]
```

:::tip[Important]
Make sure to set the [initial PVC size](../usage/persistence-and-backup#default-pvc-size) large enough to store the extracted data.
Make sure to set the [initTimeout](../reference/crds#persistence) long enough to allow init container have enough time to extract the tarball data.
:::

### Notes
- The application’s home directory is located at `/home/app`.
- The data directory is located at `/home/app/data`.
- A temporary shared volume is available for all initialization containers at `/temp`.
