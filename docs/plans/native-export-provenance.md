# Native export source provenance

**Implement the authority, source hold, mounted-worker check, result binding, and descriptor publication as one inseparable protocol.** Reuse the completed `UploadWithResult` implementation from `88da3a6`; it proves archive bytes, not snapshot provenance.

The complete remaining scope is likely larger than the earlier estimate. I recommend a first usable protocol PR for **S3 with Secret-backed credentials**, followed by provider/authentication expansion using the same protocol. Both remain upstream Cosmopilot work; Nodezi A/B and its restore-disabled wire contract stay unchanged.

## Root Cause / Goal

Accepted implementation baseline: `ec70a11e2e195b526bd61f5fb0c4d38bf7c39727` on `feat/native-export-provenance`, stacked on `feat/export-archive-results` (PR #250). The source investigation used `88da3a6`; the review correction changes documentation, option/cleanup coverage and progress output, without changing the result schema or source-protocol interfaces.

The [availability correction contract](native-export-provenance-review-fixes.md) supersedes this plan's initial error propagation, owner-deletion cleanup authorization, terminal capacity accounting and failed-export retention behavior. Its added stopped-cleanup fields preserve every upload provenance and immutable-evidence rule.

The completed slice already provides:

- Context-aware concrete `UploadWithResult` methods.
- Exact final key, compressed-byte SHA-256, size, compression and layout.
- Explicit `cleanupRequired` after acknowledged final output.
- Optional standalone CLI `--result-file`, bounded to 3,072 bytes.
- Stream/worker joining and the GCS composition corrections.

Do not recreate or redesign those interfaces in the provenance PR. The coordinator has read the complete plan and refreshed its parent to the reviewed PR #250 correction.

The actual remaining gaps are:

| Source boundary | Verified behavior and implication |
|---|---|
| `internal/datasnapshot/snapshot.go:155` | `ensureUploadResources` creates the Job before its PVC. Existing creation/adoption helpers may replace resources by name. Neither is suitable for immutable attempts. |
| `internal/datasnapshot/{s3,gcs}.go` | Clone PVCs refer to a VolumeSnapshot by name. Jobs have no mounted-worker authority check or provenance result envelope. |
| `internal/controllers/chainnode/snapshot_exports.go:592` | `mutateSnapshotExportStatus` uses `r.Get`, retries callbacks, and lacks an expected ChainNode UID check. Native authorization must use uncached reads and narrower transitions. |
| `internal/controllers/chainnode/snapshots.go:1172` | Export creation still uses the legacy provider path and has a fallback when status persistence is unavailable. That fallback must never apply to the new protocol. |
| `internal/controllers/chainnode/snapshots.go:1356` | Completion records `Uploaded` and cleans resources without an exact successful Pod/result provenance check. |
| `internal/controllers/chainnode/snapshots.go:44` | Policy-disabled handling does not reconcile the full upload lifecycle. |
| `internal/controllers/chainnode/controller.go` | Data initialization and other early returns precede ordinary snapshot reconciliation. NotFound currently returns without orphan recovery. Jobs, clone PVCs and held snapshots lack the required dedicated event mapping. |
| `internal/controllers/chainnodeset/resource_cleanup.go:257` | Parent cleanup can remove the child’s existing durable-resource finalizer. Export authority needs its own finalizer, owned exclusively by the ChainNode export path. |
| `cmd/dataexporter/cmd/{s3,gcs}.go` | Provider pre-runs construct cloud clients before `upload.RunE`. A gate added only inside that RunE is too late. |
| `internal/controllers/chainnode/restore.go:43` | Restore assigns its ServiceAccount to the whole initialization Pod, including application commands. Export identity is not automatically a safe restore identity. |
| `internal/controllers/chainnode/snapshots.go:1004` | `data-height` is a pre-capture observation. It is not an exact dataset-height measurement. |

The native guarantee should be precise:

> An accepted descriptor identifies the completed compressed bytes exported by one selected worker from a clone bound to the recorded VolumeSnapshot incarnation, under a frozen export recipe.

It does **not** prove never-signing history, application compatibility, exact dataset height, immutable cloud credentials, or safe reuse of an export ServiceAccount for restore.

Also distinguish the export’s **ChainNode authority owner** from historical snapshot creation provenance. Current snapshot discovery is name/label based; merely recording the exporting ChainNode UID does not retroactively prove which ChainNode incarnation originally captured an older snapshot. Nodezi C must retain the necessary capture/lifecycle evidence independently.

## Approach

### 1. Delivery boundaries

The authority lifecycle cannot safely be divided into “hold first, worker check later” or “digest now, descriptor validation later.”

Recommended slices:

| Slice | Complete usable boundary |
|---|---|
| **P1 — Native source protocol, S3 Secret-backed** | A configured native export acquires the source hold, creates immutable resources, authorizes the actual mounted worker, uploads through the existing result API, publishes a validated descriptor, and recovers cancellation/cleanup. All race and compaction rules below apply. |
| **P2 — Provider and authentication expansion** | Add GCS Secret-backed exports and then supported ServiceAccount/ambient export authentication to that same protocol. Each supported combination includes routing validation, worker execution, descriptor identity, cleanup, and acceptance tests. |
| **Later authorized release work** | Publish compatible images/operator/chart and update defaults. |
| **Separate Nodezi C work** | Consume released descriptors and establish never-signing, network, height/application and restore-identity eligibility. |

P1 should reject unsupported new-protocol combinations explicitly. Existing legacy GCS and ServiceAccount/ambient exports continue under their existing contract.

This gives P1 a real operational outcome without shipping unused RPCs or an authorization gate with no consumer.

### 2. Explicit compatibility selection

Add an optional `ExportTarballConfig.protocol`:

- Omitted or `legacy`: existing behavior.
- `source-v1`: the complete new protocol.

This is an executable export mode, not a dormant feature flag. It is needed because the current default exporter image is still `ghcr.io/voluzi/dataexporter:2.2.1`, and no default-image change is authorized.

For P1, `source-v1` requires S3 credentials from the configured Secret and no configured cloud ServiceAccount. Validation must exist in both the CRD and the existing Go validation path.

The operator’s existing exporter-image override supplies a compatible image during acceptance and eventual controlled rollout. Selecting `source-v1` with an old binary must fail closed on an unknown native command; there must be no fallback to legacy upload.

### 3. Permanent authority and compactable attempts

Use the existing ChainNode controller and ChainNode status. Add no controller, external database or CRD kind.

I recommend a bounded authority object separate from the existing export catalog:

```text
ChainNode.status.snapshotExportAuthority:
  version: 1
  lastSequence: int64
  attempts: map-list keyed by sequence
```

Each attempt contains:

```text
sequence, id, exportID
phase, reason
recipe, recipeSHA256
sourceHoldResourceVersion
jobCreateIssued, pvcCreateIssued
jobIssuerToken, pvcIssuerToken
job, scratchPVC, scratchPV, pod
createdAt, authorizedAt, completedAt
localCleanupState, remoteCleanupState
```

The full authority key is:

```text
namespace + ChainNode UID + sequence + random attempt UUID
```

Use a full UUID, not the existing short export identity hash.

`lastSequence` is permanent for that ChainNode UID, survives compaction, and never decreases. A newly inserted attempt must have a sequence greater than the previous watermark. Existing attempt IDs, sequences and recipes are immutable. A same-name replacement ChainNode has a different authority identity.

Use a maximum of **32 retained attempt records per ChainNode**. Pending or uncertain cleanup consumes capacity; do not silently evict it. Capacity exhaustion prevents reservation of another attempt but must not prevent invalidation or cleanup.

Add CRD transition validation for:

- Authority cannot disappear once initialized.
- Watermark cannot decrease.
- Newly inserted sequence exceeds the old watermark.
- Existing attempt identity and recipe cannot change.
- Issuance witnesses cannot revert.
- Bound resource identities are write-once.
- Terminal attempts cannot become active.
- Only terminal records may be compacted.

Keep these rules inside the new bounded authority schema; do not impose a new restrictive maximum on the existing legacy `snapshotExports` list.

The controller remains the only authority writer. Workers only read it.

**Mutation rules**

Introduce explicit operations such as:

```go
reserveSnapshotExportAttempt(...)
transitionSnapshotExportAttempt(...)
invalidateSnapshotExportAttempt(...)
publishSnapshotExportArchive(...)
compactSnapshotExportAttempt(...)
```

Each operation:

1. Uses `APIReader` for the current ChainNode.
2. Requires the expected ChainNode UID.
3. Checks the expected attempt identity and current phase.
4. Performs an RV-conditional status update.
5. Treats a missing attempt as denial, never an instruction to recreate it.

Reservation allocates a new sequence/UUID against a specific fresh state. A conflict restarts the reservation decision; it must not replay a stale closure that inserts the old identity.

All subsequent transitions require an existing record. They never upsert.

A status-write timeout requires uncached readback. If the intended transition cannot be confirmed, do not proceed with its dependent action.

Update the existing export-status merge helper to preserve the new authority alongside `snapshotExports`. Do not refresh a caller’s RV while leaving it with stale authority. Audit the surrounding full-status writers; stale full updates must conflict or fail the transition rules rather than resurrecting state.

### 4. Frozen recipe and descriptor schema

Keep new types in `api/v1/common_types.go`, so the existing documentation generator can discover them without a generator redesign.

**Immutable attempt recipe**

Freeze:

- Namespace and exporting ChainNode name/UID.
- Export ID.
- Snapshot name/UID.
- Bound VolumeSnapshotContent name/UID and CSI driver.
- Observed snapshot readiness and restore-size requirements.
- Clone PVC specification, including the resolved storage-class selection.
- Attempt-specific resource names and object basename.
- Original destination: provider, bucket, region, endpoint, path-style setting.
- Original authentication references.
- Compression, size limit, part/chunk/buffer sizes and concurrency.
- Exporter image, fixed worker command, data mount path and relevant Pod settings.
- Retention/deletion promise.
- Optional observed height and its provenance.

The recipe must contain no secret values. Its SHA-256 identifies its canonical encoding; it is not a substitute for validating its contents or for archive SHA-256.

Persist the recipe as a bounded canonical JSON string, decoded into one shared typed recipe by the controller and worker. Enforce the 16 KiB byte bound, strict decoding and canonical re-encoding equality in Go; hash the exact UTF-8 contents rather than the enclosing JSON-string encoding. CEL compares the immutable string instead of embedding Kubernetes Pod/PVC schemas. Aggregate status budgeting counts the actual escaped serialized authority and reserves terminal/descriptor capacity. The recipe remains a validated fixed native contract, never an arbitrary raw Kubernetes object.

Use attempt-specific Kubernetes names such as `export-<uuid>` rather than extending potentially long ChainNode names. Use an attempt-specific object basename; retries never reuse an earlier key.

**Identity references**

Record:

```text
mode: Secret | ServiceAccount | Ambient
namespace
credentialsSecret: name, UID, key or fixed S3 key set
executionServiceAccount: name, UID
```

These describe references used for execution. They do not assert that credential content or the cloud principal can never rotate.

For S3 Secret mode, expose only:

- `AWS_ACCESS_KEY_ID`
- `AWS_SECRET_ACCESS_KEY`
- Optional `AWS_SESSION_TOKEN`

Do not use arbitrary `envFrom`.

**Published descriptor**

Add optional `provenanceVersion` and `archive` to `SnapshotExportStatus`.

The immutable version-1 descriptor contains:

```text
version
authorityOwner: namespace, ChainNode name/UID
exportID, attemptID, attemptSequence, recipeSHA256
objectKey, sha256, sizeBytes, compression
source: snapshot/content identity, CSI driver, hold observation
destination
readIdentity
job, pod, scratchPVC, scratchPV
completedAt
optional sourceHeight + sourceHeightKind
```

Use `sourceHeightKind: CaptureObserved`; omit height when absent or invalid. Do not invent an exact-height field.

Store cleanup state outside the immutable descriptor. Successful bytes plus unfinished temporary-object cleanup can have a valid descriptor and a separate `remoteCleanupState: Required`.

A split result can complete the native export lifecycle, but produces **no single-archive descriptor**.

Bound each canonical recipe and descriptor to **16 KiB encoded JSON**. Apply an aggregate native-status budget before new reservation, leaving space for terminal/cleanup transitions. Exhaustion must be visible and must retain existing evidence.

### 5. Source hold before any export-resource POST

Add:

- ChainNode finalizer: `cosmopilot.voluzi.com/export-authority`.
- VolumeSnapshot finalizer: `cosmopilot.voluzi.com/export-source`.
- Job and scratch-PVC finalizer: `cosmopilot.voluzi.com/export-attempt`.

The ChainNode finalizer must be confirmed before reserving native attempts.

Acquire the snapshot hold by:

1. Reading the exact VolumeSnapshot uncached.
2. Requiring its expected UID and no deletion timestamp.
3. Validating readiness and exact content binding.
4. Adding the source finalizer and an attributable holder entry using an RV-conditional update.
5. Confirming that update, including readback after an uncertain response.
6. Persisting the observed held snapshot RV in authority.

Holder attribution includes namespace, ChainNode name/UID, sequence and attempt ID. It is cleanup attribution, not authorization.

Only a confirmed `Held` attempt may issue either a scratch-PVC or upload-Job POST.

Use a holder set if multiple attempts can overlap. Removing one holder must not remove another attempt’s protection. Finalizer/holder mutations require UID and RV checks.

A late hold write after cancellation may retain an object temporarily. It must not revive authority, and orphan reconciliation must remove it.

The VolumeSnapshot finalizer excludes same-name replacement while held. It does **not** guarantee continued existence of the CSI backing data. Do not change snapshot-content deletion policy or claim that an additional content finalizer is a physical-storage lease.

### 6. Resource issuance and exact binding

Use:

```text
Reserved → Held → ResourcesRequested → Uploading → Succeeded
    └──────── any nonterminal state ───────────→ Invalidated
```

`Succeeded` and `Invalidated` permanently deny new worker authorization.

For each resource:

- Persist its `*CreateIssued` witness before POST.
- Persist a fresh random per-resource issuer token in the same transition. Tokens are write-once. After an uncertain acknowledgement, uncached readback must confirm this invocation's token; an existing witness or token never permits another POST.
- Only the reconcile that confirms winning that issuance transition may issue the POST.
- Include the attempt finalizer, exact attribution and ownership in the **initial body**.
- Never recreate that attempt’s resource after its issuance witness exists.
- Recover a lost acknowledgement by GET and exact recipe/ownership validation.
- Freeze the returned UID once; never rebind it.

Preserve Job-first creation if desired, since the PVC can then carry the exact Job owner UID. The source hold already precedes both operations.

A restart after recording issuance but before sending POST may cost an attempt. After bounded recovery, invalidate it and allocate a new attempt rather than resetting its witness. A delayed original POST remains possible and must be handled by the worker gate and orphan cleanup.

The Job must have:

- One completion, parallelism one.
- `RestartPolicyNever`.
- No completion TTL.
- No automatic logical retry through replacement Pods.
- One fixed exporter container.
- Read-only archive data mount.
- No project-controlled init container.
- `TerminationMessagePolicy: File`.
- Fixed native worker invocation.

Before transitioning to `Uploading`, the controller verifies and freezes:

- Job name/UID and exact ownership.
- Scratch PVC name/UID, expected data source and specification.
- Bound PV name/UID.
- PV claimRef including scratch-PVC UID.
- Expected CSI driver and acceptable binding.
- Still-held original snapshot/content UIDs and reverse binding.
- One exact Pod UID controlled by the recorded Job UID.
- The admitted Pod’s archive-affecting command, image, mounts, credentials and execution identity.

Admission changes must be examined on the actual Pod. Unexpected containers or writable access to the data volume must deny authorization. Explicitly accommodate only necessary, validated token/workload-identity injection.

A replacement Job/PVC/Pod is not an automatic continuation. Invalidate and reserve a new logical attempt.

### 7. Mounted worker and cloud-client construction

Add a root CLI command:

```text
dataexporter native-upload
```

It accepts only authority location/identity and downward-API worker identity. It obtains destination and upload settings from the immutable authority recipe.

This avoids changing the legacy provider pre-runs and ensures the new path has this order:

```text
parse invocation
→ establish Kubernetes client
→ uncached authority GET
→ validate selected mounted worker and frozen recipe
→ construct cloud client
→ inspect/read archive directory
→ UploadWithResult
→ write bounded provenance result
```

The command executes in the actual upload container, after Kubernetes has mounted its volumes. An init-only check is insufficient.

Authorization requires:

- Expected namespace and ChainNode UID.
- Expected attempt sequence/UUID and export ID.
- `Uploading` phase.
- Exact selected Pod UID from downward API.
- Frozen Job/PVC/PV references.
- Matching recipe digest and supported protocol version.
- No deleting authority owner.

`Reserved`, `Held` or `ResourcesRequested` may wait with a bounded deadline. Missing authority, terminal state, unknown version, conflict, timeout or API failure denies work.

Use an uncached GET with resourceVersion unset. Do not authorize from an informer, projected status/configuration or `resourceVersion=0`.

No `os.Stat`, directory walk, compression, credential-provider/cloud-client initialization or object-store operation may occur before the successful check.

For P1, construct the S3 client with explicit Secret-derived credentials and explicitly ignore SDK-configured endpoint overrides. The frozen endpoint/region controls routing. Make any additional `S3Config` options additive with zero values preserving legacy behavior.

P2 must similarly constrain GCS endpoint/emulator overrides and document the supported ambient/workload-identity contract.

**Cancellation semantics**

- Gate succeeds before invalidation: the worker already has the original held clone mounted. It may finish or leave remote residue, but cannot become an accepted descriptor after invalidation.
- Gate occurs after invalidation: zero archive reads and zero object-store operations.
- A delayed PVC may still provision or mount after cancellation. This protocol prevents its worker from exporting replacement data; it does not make the CSI provisioner enforce expected snapshot UID.

Do not claim prompt cancellation of arbitrary blocked filesystem reads. Preserve the completed slice’s joining behavior and document that local filesystem I/O can delay return.

### 8. Bounded provenance result

Keep standalone `--result-file` output unchanged. Native workers write a separate envelope containing the existing `UploadResult`:

```json
{
  "version": 1,
  "ownerUID": "...",
  "exportID": "...",
  "attemptID": "...",
  "attemptSequence": 1,
  "recipeSHA256": "...",
  "jobUID": "...",
  "podUID": "...",
  "scratchPVCUID": "...",
  "scratchPVUID": "...",
  "result": {}
}
```

The complete encoded envelope, including nested result, is limited to **3,072 bytes**.

Use a shared production contract in `internal/exportprotocol` for encoding, strict decoding, identity comparison and canonical recipe hashing.

Before reserving work, check the worst-case encoded envelope using the actual expected key and bounded identity fields. Count JSON-escaped bytes, not just string lengths. Do not upload first and discover that a valid result cannot fit.

Strict decoding must reject:

- Oversize or truncated input.
- Unknown fields at every envelope/result level.
- Duplicate JSON keys, including escaped equivalent names.
- Multiple top-level values or trailing non-whitespace.
- Invalid UTF-8, wrong types, missing required fields and null substitutes.
- Unsupported versions/layouts/compression.
- Invalid digest or size.
- Single/split field inconsistencies.
- Any authority/resource identity mismatch.
- A single-object key different from the frozen expected key.

`DisallowUnknownFields` alone does not reject duplicate keys. Use token-level duplicate detection plus typed validation.

Result-file write failure exits unsuccessfully. Never parse logs as result evidence.

A known final acknowledgement followed by cancelled/failed temporary cleanup remains a completed result with `cleanupRequired`, as specified by PR250. It must not trigger a repeated upload to that key.

### 9. Publication before cleanup

Accept a result only when all of these hold:

- Current attempt is still `Uploading`.
- Exact recorded Job completed successfully.
- Exact selected Pod and exporter container terminated successfully.
- Strict result envelope matches every recorded identity.
- Scratch PVC/PV binding is unchanged.
- Source snapshot/content identities and hold remain valid.
- Result validates against the frozen recipe.

Publish the descriptor, terminal `Succeeded` state and export completion in one RV-conditional status update.

If invalidation wins first, publication fails. If publication acknowledgement is lost, read back and require the identical terminal descriptor.

Only confirmed terminal state permits source-hold release and ephemeral cleanup. A publication failure leaves holds and result resources available for recovery.

Cleanup uses UID-preconditioned deletion and UID/RV-conditional finalizer removal. Native resources must never enter the legacy replacement/adoption or basename-only cleanup paths.

Confirmed native exports honor their frozen `DeleteOnExpire` policy through a dedicated cleanup path using the original destination and credential identity. A single-object descriptor provides the exact deletion key. For acknowledged S3 split output, generate exactly `basename-part-%08d<compression extension>` for indices from zero through `ObjectCount - 1`, matching the uploader. Native cleanup performs no object listing or prefix deletion. Generate work lazily with bounded concurrent workers, propagate cancellation and join them, and retain incomplete cleanup after any failed deletion. Possible late writes from an invalidated attempt keep remote cleanup required even after a deletion acknowledgement or empty listing.

Retain the strict successful upload result on the attempt, including split layout. Native deletion uses bounded, monotonic cleanup authority: a write-once issuer token, exact cleanup Job and selected Pod identities, and acknowledged completion. A fixed `native-cleanup` worker reads current authority before creating the S3 client. It requires the exact owner UID, terminal `Succeeded` attempt, matching catalog record in `Deleting`, and its selected cleanup Pod UID. This cleanup authorization may remain valid while that exact owner is deleting; upload authorization may not. Preserve the original Secret identity and fixed credential keys. Record a matching successful Job/Pod acknowledgement before deleting local cleanup resources or compacting the attempt. Invalidated attempts with uncertain output remain outside this completion path.

Make `snapshotExports` a map-list keyed by its existing required `id` so descriptor transitions correlate correctly. Verify existing multi-record legacy history remains writable through the API server, without a new list maximum. Validate native descriptor immutability and stale-worker denial through that same boundary. If CEL cost cannot be bounded without limiting legacy history, revise the validation design before proceeding.

Keep published descriptors after the VolumeSnapshot disappears when the archive is retained. Existing pruning based on snapshot/resource absence must not discard them.

ChainNode deletion ultimately removes its status. This proposal does not create an external permanent catalog; Nodezi must retain accepted descriptors before relying on them across authority-owner deletion.

### 10. Recovery and deletion ordering

Run native attempt recovery early in `Reconcile`, before data initialization, snapshot-policy exits and ordinary snapshot discovery.

Cover:

- Disabled snapshot/export policy.
- Removed or changed export configuration.
- Snapshot missing or deleting.
- Changed credentials/reference identity.
- ChainNode deletion.
- ChainNodeSet group deletion and root deletion.
- Namespace termination.
- Controller restart.
- Same-name authority replacement.

Policy disable cancels active native attempts through terminal invalidation; re-enable may allocate a new sequence. Existing frozen destinations remain authoritative for cleanup.

The dedicated ChainNode export finalizer prevents parent cleanup from removing authority before native invalidation. ChainNodeSet cleanup must preserve it and wait for the child export path; it must not synthesize native completion or release source holds.

Add event mappings for native Jobs, their Pods, scratch PVCs and held snapshots. Attribution must carry owner UID as well as name. Initial informer lists and late-arriving resources must both trigger recovery.

NotFound and same-name/different-UID handling may remove attributable orphan holds and exact ephemeral resources, but must never reconstruct authority from labels, annotations or Job specs.

Cached owner absence or an owner snapshot predating a reservation cannot authorize orphan cleanup. After observing each candidate resource or holder, read the current authority uncached and fail closed on API errors before taking destructive action. Check cached ChainNode `NotFound` against the API before entering owner-absent cleanup. Retain UID/RV preconditions and every unrelated holder during mutations.

**Compaction**

Compact an attempt only after:

- It is terminal.
- A successful descriptor, if any, is durably published.
- Required cleanup evidence has been retained.
- Known local resources have been handled.

The permanent watermark stays. Late resources referring to compacted attempts receive missing-authority denial and cleanup. Absence does not permit recreation or prove that all delayed operations have disappeared.

Keep `remoteCleanupState: Required` when authorized writes or final completion remain uncertain. A quiet interval, empty listing or cleanup acknowledgement is not proof that an arbitrarily delayed write cannot arrive.

### 11. RBAC and identity boundary

For P1, create a dedicated export-worker ServiceAccount per authority owner, separate from the application ServiceAccount.

Provide a Role/RoleBinding permitting only:

```yaml
apiGroups: ["cosmopilot.voluzi.com"]
resources: ["chainnodes"]
resourceNames: ["<exact-owner-name>"]
verbs: ["get"]
```

The worker receives no status update, Secret GET, resource mutation or general listing permission. Supply its Kubernetes token explicitly to the fixed worker; keep cloud credentials confined to that container.

The controller needs additional permissions for:

- PV and VolumeSnapshotContent reads.
- StorageClass reads if resolving default clone storage class.
- Dedicated ServiceAccount creation/deletion.
- Namespaced Role/RoleBinding lifecycle.
- Existing resource-finalizer mutation paths.

Generate the Helm ClusterRole from markers. Do not grant broad `bind` or `escalate`; the controller already possesses the ChainNode GET permission it delegates.

Validate permissions using an impersonated worker identity in envtest. An admin test client is not RBAC evidence.

For future ServiceAccount/ambient export modes, record the original identity and verify the actual admitted Pod. That does not authorize reusing it for restore.

The future initial restore path should isolate a Secret-backed read credential in a fixed restore container. Current restore assigns the source ServiceAccount to the whole init Pod; project-controlled initialization must not inherit privileged export workload identity.

## Critical Files

P1 ownership is concrete:

| Files | Changes |
|---|---|
| `api/v1/common_types.go` | Protocol selection, authority/attempt/recipe/descriptor types, bounds and transition markers. |
| `api/v1/chainnode_types.go` | Authority field and native lifecycle conditions. |
| `api/v1/common_helper.go` | Protocol/provider combination validation and compatibility default. |
| `internal/exportprotocol/protocol.go` **new** | Canonical recipe digest, bounded envelope, strict decoder and shared validation. |
| `internal/exportprotocol/protocol_test.go` **new** | Independent JSON/wire-contract regressions. |
| `cmd/dataexporter/cmd/native_upload.go` **new** | Actual mounted-worker authorization, then provider construction/upload/result writing. |
| `cmd/dataexporter/cmd/native_upload_test.go` **new** | Real CLI execution boundary with HTTP API/store fixtures. |
| `Dockerfile.dataExporter` | Add API, internal/exportprotocol and transitive pkg/images source inputs needed by the native worker; preserve image defaults and build behavior. |
| `pkg/dataexporter/s3.go` | Additive explicit credentials/routing controls needed by native execution; no hash/stream redesign. |
| `pkg/dataexporter/s3_test.go` | Native routing/credential override regression only; reuse existing byte fixtures. |
| `internal/datasnapshot/export_attempt.go` **new** | Native resource rendering, initial finalizers and exact resource identities. |
| `internal/datasnapshot/snapshot.go` | Keep native resources out of legacy adoption/replacement/list-cleanup paths. |
| `internal/datasnapshot/s3.go` | Native S3 recipe/resource adapter where shared provider settings are required. |
| `internal/datasnapshot/export_attempt_test.go` **new** | Security-relevant rendered Job/PVC contract. |
| `internal/controllers/chainnode/snapshot_export_attempts.go` **new** | Reservation, hold, issuance, binding, invalidation and publication. |
| `internal/controllers/chainnode/snapshot_export_attempt_cleanup.go` **new** | Terminal cleanup, compaction and orphan recovery. |
| `internal/controllers/chainnode/snapshot_export_watch.go` **new** | Native resource-to-owner event mapping. |
| `internal/controllers/chainnode/snapshot_exports.go` | Authority-preserving status mutations and descriptor-aware retention/deletion. |
| `internal/controllers/chainnode/snapshots.go` | Protocol dispatch; exclude native attempts from legacy finish/retry/cleanup paths. |
| `internal/controllers/chainnode/controller.go` | Early recovery, NotFound handling, watches and RBAC markers. |
| `internal/controllers/chainnode/resource_cleanup.go` | Native terminalization before authority-finalizer release. |
| `internal/controllers/chainnodeset/resource_cleanup.go` | Preserve/wait for child export authority during parent cleanup. |
| `internal/controllers/chainnode/snapshot_export_attempts_test.go` **new** | Focused state/identity regressions that do not duplicate envtest. |
| `internal/controllers/chainnode/snapshot_export_destination_test.go` | Legacy compatibility and descriptor-retention regressions. |
| `test/integration/snapshot_export_attempt_test.go` **new** | Real API-server CAS, finalizers, admission, RBAC and lifecycle races. |
| `test/integration/snapshot_export_faults_test.go` **new** | Test-owned HTTP fault/delay fixture around the real API endpoint. |
| `test/framework/helpers.go` | Correctly populate needed simulated snapshot/content binding fields, including restore size. |
| `test/e2e/chainnode_export_restore_test.go` | Consume descriptor key/digest; retain independently measured bytes as assertions. |
| `test/e2e/snapshot_export_provenance_test.go` **new** | Real CSI mounted-worker and delayed-create acceptance. |
| `docs/docs/reference/cli.md` | Native command and strict result semantics. |
| `docs/docs/usage/persistence-and-backup.md` | Protocol, lifecycle, retention and operational limits. |
| `docs/docs/usage/restoring-from-snapshot.md` | Descriptor scope, height and read-identity restrictions. |
| `docs/plans/native-export-provenance.md` **coordinator-owned artifact** | Save the accepted plan before implementation. |

Generated outputs:

- `api/v1/zz_generated.deepcopy.go`
- `helm/cosmopilot/crds/cosmopilot.voluzi.com_chainnodes.yaml`
- `helm/cosmopilot/crds/cosmopilot.voluzi.com_chainnodesets.yaml`, because the shared export spec is embedded there.
- `helm/cosmopilot/templates/rbac/role.yaml`
- `docs/docs/reference/crds/crds.md`

P2 adds/extends:

- `internal/datasnapshot/gcs.go`
- `pkg/dataexporter/gcs.go`, only for native client construction/routing needs.
- Native worker, identity validation and corresponding provider acceptance tests.
- Protocol-combination validation and documentation.

No protobuf or Nodezi wire changes are required.

## Verification and Executable Acceptance

No tests were run in this planning task.

**Local, without containers or real CSI**

Existing fixtures are suitable starting points:

- S3/GCS HTTP byte fixtures in `pkg/dataexporter`.
- Real envtest API server in `test/framework/envtest.go`.
- Snapshot/content simulation in `test/framework/helpers.go`.
- Existing initialization integration tests show how to avoid blocking on absent kubelet execution.

Envtest has no CSI provisioner, kubelet, Job controller or ordinary garbage collector. Tests must explicitly identify any Pod/status/binding simulation. They cannot claim physical clone or mount correctness.

Required local acceptance:

| Scenario | Observable pass condition |
|---|---|
| Hold ordering | No export Job/PVC POST reaches the API before confirmed snapshot UID/RV hold and durable issuance. |
| Concurrent reconciles | Only one issuance winner posts; stale transitions cannot overwrite terminal state or rebind UIDs. |
| Lost hold/status acknowledgement | Readback either confirms the exact transition or dependent creation does not occur. |
| Lost create acknowledgement | Exact resource is recovered, or attempt invalidates; no old-resource recreation. |
| Delayed POST | Pause forwarding, invalidate, release hold, replace source name, then forward. Native worker denies before directory access or store requests. |
| Compaction | Remove terminal attempt, preserve watermark, deliver stale reservation/status writes and late resources. Old authorization cannot return. |
| Resource substitution | Different Job, Pod, PVC or PV UID prevents authorization/publication. |
| Missing authority | Missing owner, wrong owner UID, missing attempt and unsupported version all deny. |
| Source/content loss | Deleting/replaced/missing source or broken reverse binding prevents publication and drives terminal recovery. |
| Policy/group deletion | Invalidation occurs despite disabled policy, early initialization exits or parent cleanup. |
| Publication fault | Valid completed result plus failed status write retains source hold and result resources; retry publishes once before cleanup. |
| Malformed result | Unknown, duplicate, oversized, truncated, trailing or mismatched JSON produces no descriptor. |
| Split output | Export can finish, but no single-archive descriptor appears. |
| Cleanup uncertainty | Known final acknowledgement plus cleanup cancellation preserves descriptor and records cleanup required. |
| RBAC | Worker identity can GET its exact owner and cannot GET another owner, read Secrets or mutate status. |
| Routing | Secret/environment endpoint overrides cannot redirect native S3 traffic. |
| Legacy compatibility | Existing API/CLI results and legacy export lifecycle retain their contract. |

For “zero reads before denial,” use a real inaccessible/nonexistent test source path and a real HTTP request recorder. Assert the authority denial and zero store requests; do not replace the production uploader with a mock that supplies the result.

Fault fixtures should delay or drop actual HTTP requests/responses to envtest, not imitate resourceVersion or finalizer behavior in a fake client.

Suggested focused commands after implementation:

```sh
go test ./internal/exportprotocol ./cmd/dataexporter/cmd \
  ./internal/datasnapshot ./internal/controllers/chainnode \
  ./internal/controllers/chainnodeset ./pkg/dataexporter

KUBEBUILDER_ASSETS="<verified-local-assets>" \
  go test ./test/integration/... -timeout 10m \
  -ginkgo.focus="Native export attempt"

go build ./cmd/dataexporter ./cmd/...
go vet ./internal/exportprotocol ./cmd/dataexporter/cmd \
  ./internal/datasnapshot ./internal/controllers/chainnode
```

Use the repository’s generation commands and review generated diffs. Available local envtest binaries were observed, but their suitability for the new CEL rules must be verified; their presence is not a passed acceptance run.

**Necessary live acceptance gaps**

A real CSI cluster must prove:

- Worker execution actually occurs after mounting the intended clone.
- Both sides of the gate/invalidation race.
- Late Job/PVC creation after cancellation.
- Snapshot/content deletion and storage-driver behavior.
- Actual Job replacement/GC/finalizer behavior.
- Descriptor-driven restore to a fresh PVC.
- Wrong digest fails initialization without starting the serving node.

Real cloud acceptance must separately cover supported S3/GCS service behavior and any ServiceAccount/ambient identity mode. HTTP fixtures and MinIO do not establish IAM/workload-identity correctness.

The live test should use the descriptor’s key and digest as the restore recipe. Independently calculate actual object SHA-256 only as an assertion.

Any future container-backed acceptance must follow the task-owned inventory and cleanup rules. This planner created none.

## Risks & Side Effects

- **Trust boundary:** this is provenance from an identified VolumeSnapshot to completed archive bytes. It does not independently establish the snapshot’s entire capture/signing history.
- **Storage assumptions:** Kubernetes identity/binding semantics and conforming CSI behavior remain assumptions until live acceptance. Forced finalizer removal and privileged storage tampering are outside the guarantee.
- **Cancellation:** a worker authorized before cancellation may finish remote work. Do not advertise immediate cancellation or definitive remote deletion after a quiet interval.
- **Filesystem I/O:** joining is preserved; regular filesystem reads may not be promptly interruptible.
- **Capacity:** unresolved attempts and retained descriptors consume status space. Stop new reservations at capacity rather than losing evidence.
- **Operator outage:** holds/finalizers remain until recovery. Document diagnosis and cleanup acknowledgement accurately.
- **Admission:** automatic injection can alter the worker Pod. Validate the actual admitted execution shape.
- **Downgrade:** old controllers cannot safely own active new-protocol authority. Drain/invalidate active attempts and preserve the additive schema before any rollback.
- **Schema order:** install/update CRDs before the supporting controller. Helm installation of a chart does not by itself establish that existing CRDs were upgraded.
- **Consumer identity:** original export credentials are retained references, not permission to reuse a privileged ServiceAccount in application initialization.
- **Height:** `CaptureObserved` remains advisory; no exact application database-height claim.
- **Owner deletion:** native status is not an independent long-lived catalog. Nodezi’s retained descriptor contract is still needed.

No unresolved product decision is needed to prepare P1. Provider expansion and release authorization are explicit later boundaries.

## Scope

**Complete remaining protocol across both providers and identity modes:** approximately **36–43 authored files and 4,500–6,500 changed lines**, including meaningful tests, plus generated outputs; high complexity.

**Recommended P1:** approximately **30–34 authored files and 3,300–4,500 changed lines**, plus generated outputs. The hold/authority/gate/publication protocol stays whole. P2 adds provider/authentication combinations through complete adapters rather than weakening P1.

Out of scope:

- Reimplementing PR250’s digest/result pipeline.
- Nodezi A/B or restore enablement.
- Never-signing history implementation.
- Application-aware exact-height measurement.
- CSI provisioner modifications.
- New CRD/controller/database.
- Image publication, tags, releases, default-pin changes or deployment.
- Assuming an upgraded controller is installed remotely.

Nodezi remains pinned to stable Cosmopilot **5.3.1** until an authorized release and consumer migration. An unmerged source PR or locally built image is not a released module contract.

## Coordinator delivery clarifications

- Deliver P1 as the complete S3 Secret-backed protocol. Provider/authentication expansion remains a later complete slice; legacy modes retain their contract. Nodezi A/B and its protocol-4 wire remain unchanged and restore-disabled.
- Install the additive CRDs before the compatible controller. Activation of source-v1 requires all legacy controller writers to be stopped and a compatible exporter-image override selected. Do not rely on old binaries interpreting the new selector safely, and do not run mixed legacy/new writers over active authority.
- Preserve the seven PR #250 guarantees: complete stored-byte identity; acknowledged completion; no successful result on uncertain output; split exclusion; local contexts and joined workers; post-ack cleanup cancellation retaining known success; and explicit kernel-filesystem latency limits.
- Keep the five pre-existing TEST documents unchanged. No tag, release, default image update, merge, production deployment or released-module claim is authorized. Existing CSI/cloud fixtures are simulations unless actual mounted execution and object-store behavior are exercised. Full E2E matrices run in CI.
- One implementation owner covers API/schema, authority transitions, worker gate, lifecycle, RBAC, generated outputs and tests because they share the frozen protocol. Before edits, inventory exact files/protected hashes. Use focused local proofs and independent cross-vendor review. Any temporary containers require fresh exact inventory, attributed ownership and cleanup on every exit path; never invoke destructive cleanup targets.
