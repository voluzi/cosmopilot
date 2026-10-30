# Native export provenance availability corrections

Parent: `ec70a11e2e195b526bd61f5fb0c4d38bf7c39727`, with the frozen source-v1 implementation. This plan addresses the first independent review without changing archive provenance, image defaults, dependencies, or Nodezi restore behavior.

## Root Cause / Goal

Three boundaries need correction:

- Native backup failures propagate through early `recoverNativeExports` and later `ensureVolumeSnapshots`, aborting unrelated node reconciliation.
- Cleanup authority permits one immutable issuance but has no durable stopped outcome. Remote cleanup also incorrectly becomes a prerequisite for owner deletion.
- Reservation counts already-materialized terminal descriptors again as future growth, while completed native catalog records never leave status.

After correction, native work must fail closed locally, expose its condition, and retry at a bounded cadence while genesis, configuration, signing checks, Pod reconciliation and image upgrades proceed. Core owner identity and authorization failures must still stop reconciliation.

No upload witness, selected execution identity, recipe, descriptor, completed result or terminal upload phase may be reset.

## Approach

### 1. Additive status and acknowledgement contract

Use the existing attempt, catalog and condition structures. Add no spec settings, retry generations, worker framework or new controller.

Add these three optional fields to `SnapshotExportAttempt`:

| Field | Type/default | Meaning |
|---|---|---|
| `cleanupIssuedAt` | `*metav1.Time`; absent | Written atomically with `cleanupIssuer`, then immutable. Starts recovery of an issued but unobserved cleanup Job. |
| `cleanupState` | string; absent | Absent means no stopped cleanup outcome. Allowed nonempty values: `Failed`, `Acknowledged`, `Abandoned`. |
| `cleanupReason` | string; absent; maximum 256 characters | Bounded diagnostic reason for the stopped outcome. No credentials or unbounded API response bodies. |

Keep `cleanupComplete` as the existing **verified successful cleanup acknowledgement**, never an operator acknowledgement. Keep `remoteCleanupState` honest:

- Invalidated before upload authorization: `Complete`, because the gate proves zero remote upload I/O.
- Invalidated after authorization: `Required`, permanently for that invalidated attempt.
- Failed, abandoned or operator-acknowledged remote deletion: `Required` if deletion/output remains unresolved.
- A retained successful archive with no unresolved cleanup remains `Complete`; that value does not mean the archive was deleted.
- Only the existing exact successful cleanup Job/Pod acknowledgement can establish automatic deletion completion.

Add two condition constants:

- `SnapshotExportDegraded=True`: a native recovery operation is blocked. Reasons include `CleanupFailed`, `ExecutionIdentityUnavailable`, `RecoveryDeferred`, and `InvalidAuthority`. Include the relevant export ID.
- `SnapshotExportCapacityExceeded=True`: reservation was declined. Reasons `AttemptLimit`, `StatusBudget`, or `SequenceExhausted`.

Keep `SnapshotExportCleanup=True` for unresolved remote cleanup, including acknowledged uncertainty. Acknowledgement changes its reason/message to distinguish operator acceptance; it does not remove the uncertainty.

Conditions must be preserved by `mergeSnapshotExportOwnedStatus`, written only when changed, and cleared by successful reevaluation of their owning operation. Successful early recovery must not clear a capacity condition that reservation has not reevaluated.

Use a dedicated annotation:

```text
cosmopilot.voluzi.com/native-export-cleanup-ack=<export-ID>[,<export-ID>...]
```

Each token is the complete native catalog export ID, including its attempt UUID. This avoids changing the legacy acknowledgement contract.

Native acknowledgement semantics:

1. Read the exact owner uncached and require its expected UID.
2. Accept only a matching native record whose attempt is `Invalidated` with unresolved remote output, or whose `cleanupState` is `Failed`.
3. In one CAS, set `cleanupState=Acknowledged`, catalog phase `Acknowledged`, and an explicit “without deletion proof” message.
4. Preserve `remoteCleanupState=Required`, immutable execution witnesses, completed result and descriptor.
5. Remove only successfully applied or already-durably-applied tokens, using an owner-UID-checked, RV-conditional metadata update.
6. Unknown or ineligible tokens authorize nothing. Repeated application is idempotent.
7. The legacy acknowledgement loop must explicitly skip native records.

Add bounded CEL rules:

- `self.attempts.all(a, a.sequence <= self.lastSequence)`.
- `cleanupIssuedAt`, once present, cannot change or disappear.
- New cleanup issuance requires its timestamp in the same update.
- `cleanupState` cannot disappear or return to the open state. An absent state may enter any stopped state; among stopped states permit only `Failed → Acknowledged`. `Acknowledged` and `Abandoned` remain stopped.
- Nonempty `cleanupState` requires a terminal upload phase and cannot coexist with successful `cleanupComplete`.
- Once an `Invalidated` attempt has `remoteCleanupState=Required`, it cannot become `Complete`.
- Preserve `authorizedAt` once set; require it for `Uploading`. This supports reliable preauthorization classification.

Retain every existing immutability, watermark, terminal-state and compaction rule. Do not add bounds to legacy history or ChainNodeSet node lists.

### 2. Isolate native errors at both production entry paths

Introduce a small native-specific outcome/error classification, not a generic task framework.

At `controller.go`’s early `recoverNativeExports` call:

- Preserve fatal handling for failed uncached owner reads, owner UID replacement, owner authorization denial, missing required reader, and canceled reconcile context.
- Convert backup-local failures into a condition plus retry outcome: unavailable/replaced export credentials, failed cleanup Job, unexpected native resource/admission identity, native capacity, and recoverable native resource-operation failures.
- Deny the affected native action and preserve its authority. Do not interpret an error as successful cleanup.
- Continue processing independent attempts where possible, so one failed cleanup does not prevent another active attempt from being invalidated during owner deletion.
- Before continuing core work after a native failure, ensure the caller still has a fresh, matching owner. Preserve the existing metadata/spec guard on RV refresh.
- Leave all signing, reservation, application identity and core resource checks unchanged.

Do not replace the early error return with unconditional logging and continuation. Classification must originate from the failed operation, using typed errors/outcomes rather than message matching.

At `ensureVolumeSnapshots`:

- Handle expected native reservation/recovery outcomes at the native dispatch points, including all three `exportTarball` call sites and the `isTarballReady` path.
- A declined reservation must not set `exporting=true` or emit “export started”.
- Use an explicit reservation outcome such as reserved/existing/deferred; the existing “nil after installing the finalizer” must not be confused with a persisted reservation.
- Continue snapshot bookkeeping and retention after a deferred native export, rather than immediately returning from the entire snapshot loop.
- Leave legacy stale-Job handling and unrelated snapshot errors unchanged.

Keep stop-node repair ahead of native export admission. Once CSI reports the intended snapshot ready, clear the applicable snapshot/stop-node state even if export capacity or credentials are unavailable. Never clear those markers while the actual CSI snapshot is still pending.

### 3. Stop failed cleanup safely and make owner deletion independent of cloud access

Retain one cleanup Job issuance per upload attempt.

For a live owner:

- Write `cleanupIssuer` and `cleanupIssuedAt` in the same CAS before POST. Only that invocation may POST.
- Recover an uncertain POST by exact GET and validation.
- If the Job remains absent for two minutes after `cleanupIssuedAt`, persist `Failed/IssuedCleanupMissing`. Do not reissue.
- A failed Job, replacement Pod, substituted identity, invalid admitted shape or permanently unavailable original credential identity becomes `Failed` with remote cleanup still required.
- A definitive rejected POST becomes failed; an uncertain transport outcome retains the normal readback window.
- Persist the stopped state before removing cleanup resources. `AuthorizeCleanup` and the CLI’s waiting loop must immediately deny every stopped state.
- A stopped cleanup never reopens when credentials return or when a late original Job appears.
- Clean exact local resources after durable stop; retain the record and condition for acknowledgement.

Remove the inherited `DisruptionTarget: Ignore` behavior from both native Jobs. Preserve `backoffLimit=0`, one completion, one parallel worker, `RestartPolicyNever`, and `PodReplacementPolicy=Failed`. Validate the resulting native policy explicitly. Replacement Pods remain unauthorized.

Owner/namespace deletion policy:

- `DeleteOnExpire` means snapshot expiration/removal under the existing retention contract. Owner deletion alone must not initiate remote archive deletion.
- Check owner/namespace termination before credential reads or cleanup issuance.
- Stop active uploads through durable invalidation.
- Stop any incomplete cleanup authorization with `Abandoned/OwnerDeleting` or `NamespaceTerminating`, unless it is already stopped.
- Preserve unresolved deletion as `Required`; preserve a retained archive’s descriptor and existing catalog meaning.
- Deny cleanup authorization when the owner is deleting. Namespace termination also closes authority through the durable stopped state.
- Do not create Jobs in a terminating namespace.

Release the native authority finalizer only after:

1. Every upload attempt is durably terminal.
2. Every remaining cleanup authorization is completed or durably stopped.
3. Attributable source holds have been released without disturbing other holders.
4. Known upload and cleanup Jobs, selected Pods and scratch PVCs have been handled using exact UID/RV safeguards, with required local absence checks.
5. No failed authority read or unverified identity was treated as permission to destroy resources.

Apply these conditions to stopped cleanup resources as well as `cleanupComplete` resources; the existing completion-only check is insufficient.

Remote credentials, cloud responses and inability to create cleanup Jobs must not gate that finalizer. Genuine inability to persist invalidation or handle protected local resources still does.

Keep both `finalizeResources` and `finalizeTerminatingNamespace` on this same recovery contract. Preserve the ChainNodeSet guard that waits for the child’s native finalizer, and preserve all signing/quiescence safeguards.

Document that a worker authorized before cancellation can still finish remote I/O. Owner deletion destroys status; external consumers must retain accepted descriptors and unresolved-cleanup evidence beforehand. No forced finalizer removal procedure is introduced.

### 4. Repair invalidated snapshot markers and separate CSI retention from remote proof

Choose terminal native export failure for this correction, rather than introducing automatic upload retry counters.

- After durable invalidation, mark the exact source snapshot’s export annotation `failed`, using an uncached read and its expected UID/RV.
- Do this during recovery, including policy-disabled and initialization-early-return paths.
- Repair the marker before compacting a preauthorization invalidation while its source remains.
- Failed native uploads must no longer remain `exporting=true`.

Classify uncertainty inside the invalidation CAS **before** changing phase:

```text
authorizedAt exists or previous phase == Uploading → Required
otherwise → Complete
```

Keep the delayed-POST proof: an issued Job that never reached authorization may arrive later, but its gate must deny I/O after terminal invalidation.

Give native retention an explicit branch before legacy deletion-marker logic:

| Native state | CSI retention behavior |
|---|---|
| Active upload | Wait for terminalization/local safety. |
| Invalidated upload, local cleanup complete | Permit ordinary CSI expiry/count retention; retain any remote uncertainty. |
| Successful retained archive, `DeleteOnExpire=false` | Permit CSI retention; preserve descriptor/catalog. |
| Successful archive with requested automatic deletion | Await confirmed deletion, or explicit acknowledgement after cleanup failure. |
| Acknowledged failed cleanup, local resources handled | Permit CSI retention; retain `Required` history. |

Do not report “tarball deleted”, create a deletion-proof annotation, or set catalog `Deleted` merely because local snapshot deletion is allowed.

Keep native records out of every legacy provider, adoption, basename-deletion and acknowledgement-completion path. A manual later export retry must allocate a fresh sequence/UUID and pass the same reservation checks; no old attempt is reopened.

### 5. Replace the capacity arithmetic and compact only resolved native history

Keep the 32-attempt hard upper bound and the 256 KiB reservation budget.

Calculate:

```text
actual serialized current status
+ new reservation/catalog bytes
+ only still-unmaterialized lifecycle growth
+ bounded condition/diagnostic slack
```

Use production serialization and bounded projected fields. For active uploads, account for missing bindings, completed envelope, descriptor and terminal fields. For successful retained attempts, count the existing descriptor/result once and reserve only missing cleanup metadata that may still be added. Do not charge another 32 KiB for their completed descriptor.

Reuse the existing envelope/descriptor preflight construction where practical. Include JSON escaping, maximum allowed identities and cleanup metadata. Bound diagnostic messages so the reserve has a meaningful limit.

The supplied example has:

- Attempt: 3,255 encoded bytes.
- Catalog record: 3,655 encoded bytes.
- Thirty pairs: approximately 207,300 bytes before remaining growth and other status.

That supports a realistic 30-retained-export acceptance case, but does not promise 32 maximum-sized records. Document that the byte budget can impose a lower limit and existing non-native status also consumes space.

Capacity exhaustion must:

- Persist the capacity condition.
- Skip reservation without changing the watermark or adding a partial record.
- Preserve existing evidence.
- Permit normal node reconciliation, invalidation, cleanup and compaction.
- Retry conservatively.

Do not introduce a budget check that rejects safety-critical terminal or cleanup writes.

Native catalog pruning belongs in the native CAS path:

- Remove confirmed-deleted native history only after remote deletion is proven, required local cleanup is complete, and the exact source snapshot is gone or replaced.
- Delaying catalog removal until source absence prevents the still-existing snapshot from being exported again.
- Remove the eligible attempt and catalog together where both remain.
- A safe preauthorization invalidation may be compacted once local cleanup/marker repair is complete. Keep its failed catalog record while the source remains; remove it after source absence.
- A successful retained descriptor remains after attempt compaction and after source snapshot removal.
- Never prune acknowledged-but-unproven cleanup, uncertain authorized output, or retained archives.
- Preserve the permanent watermark and all stale-resurrection defenses.

Unresolved attempts and permanently retained archives can eventually exhaust this bounded catalog. That is an explicit contract limit, not a reason to erase evidence or block the node. This correction does not create archival history storage or delete Nodezi’s retained history.

### 6. Admission, polling and strict decoding

For W3, compare resource requirements separately before comparing containers:

- Every explicit recipe request and limit must remain present at an equal or greater quantity.
- Permit admission-added CPU, memory and ephemeral-storage requests/limits.
- Preserve strict equality for resource claims and other resource types unless an explicit supported policy is justified.
- Normalize only the accepted resource fields, then retain the complete existing image, command, arguments, environment, credentials, mounts, identity, security and container-count checks.
- Apply the same rule to upload and cleanup Pod validation; the cleanup validator’s direct container comparison must not bypass it.

For W5:

- Add exact attempt and owner label selectors to native Pod lists; continue verifying controller references and UIDs after listing.
- Use watches for prompt Job/Pod/PVC progress.
- Replace the native one-second full reconcile with a 30-second fallback for active work, pending local cleanup and retryable native conditions, capped by any already-shorter legitimate reconcile period.
- Do not poll retained successful archives merely because their cleanup issuer is absent.
- Preserve the owner-wide orphan safety scan, including the uncached authority check after each candidate. Do not naively label-filter shared snapshot holders in a way that loses another owner’s hold.

For S1:

- Keep token-level duplicate/null/UTF-8 detection.
- Validate exact case-sensitive envelope keys and nested `UploadResult` keys against small explicit allowed sets.
- Reject `Version`, `OwnerUID`, `CleanupRequired` and similar variants, whether alone or alongside canonical names.
- Continue accepting arbitrary JSON field order and whitespace.
- Leave canonical recipe encoding unchanged; do not add reflection-based schema machinery.

For S2, accept and document the existing conservative upload-resource recovery timeout measured from `CreatedAt`. A slow hold can cost a logical attempt. No upload issuance timestamp fields are needed in this correction.

### 7. Complete the supplemental fixes

- **N1:** strengthen the policy-disabled integration case so compaction alone cannot satisfy it. Use an actual source snapshot retained long enough for the catalog’s exact `PolicyOrOwnerChanged` reason to remain observable, or capture the terminal transition through the API. Missing-source invalidation must fail this test.
- **N2:** guard the absent S3 object explicitly in CEL. Verify the intended rejection message for source-v1/GCS through ChainNode and nested ChainNodeSet API cases. If CRD cost rejects the guarded form, revise that expression without adding legacy list bounds. Record a precise deferral if necessary; do not enter another blind patch loop.
- **N3:** synchronize the S3 route fixture’s `auth` and request counter using a mutex/atomic. Run the focused race test.

## Critical Files

One fresh Sol implementation owner should cover the whole correction.

| Files | Responsibility |
|---|---|
| [common_types.go](../../api/v1/common_types.go), [chainnode_types.go](../../api/v1/chainnode_types.go) | Three status fields, conditions, bounded CEL and selector guard. |
| [snapshot_export_attempts.go](../../internal/controllers/chainnode/snapshot_export_attempts.go) | Reservation outcomes, projected byte budget, failure classification, selected Pod queries. |
| [snapshot_export_attempt_cleanup.go](../../internal/controllers/chainnode/snapshot_export_attempt_cleanup.go) | Invalidation classification, failed/acknowledged/abandoned cleanup, marker repair, deletion policy, local teardown, compaction. |
| [controller.go](../../internal/controllers/chainnode/controller.go), [resource_cleanup.go](../../internal/controllers/chainnode/resource_cleanup.go) | Core availability and both finalization paths. |
| [snapshots.go](../../internal/controllers/chainnode/snapshots.go), [snapshot_exports.go](../../internal/controllers/chainnode/snapshot_exports.go) | Deferred reservation, native retention, condition merging, explicit legacy/native separation. |
| [export_attempt.go](../../internal/datasnapshot/export_attempt.go) | Admission resource normalization and native Job failure policy. |
| [protocol.go](../../internal/exportprotocol/protocol.go), [native_upload.go](../../cmd/dataexporter/cmd/native_upload.go) | Closed cleanup gate, strict key casing and immediate CLI denial. |
| Existing owner API test and integration native test | Lifecycle, availability, retention, admission and schema regressions. |
| Existing protocol, CLI, resource-rendering and S3 tests | Wire/gate/resource positive-negative cases and fixture synchronization. |
| [Makefile](../../Makefile) | Explicit API-boundary test invocation with assets. |
| Usage, restore and CLI documentation | Failure/acknowledgement, deletion semantics, capacity and timeout limits. |

Regenerate deepcopy, affected CRDs and reference docs. Verify RBAC generation remains unchanged unless an actual required marker changes. ChainNodeSet production cleanup should need no behavioral change.

The root owns the saved correction plan, freeze artifacts, commit and publication.

## Verification

Preserve existing proofs and add regressions at their owning boundary before fixing each behavior. Record actual failure reasons; compile/setup failures are not behavioral RED evidence.

1. **API lifecycle regressions:** extend the existing envtest boundary to cover preauthorization versus postauthorization invalidation, cleanup Job failure, lost cleanup POST acknowledgement, missing issued Job, stopped late worker denial, acknowledgement idempotency, and owner deletion with missing/replaced credentials.
2. **Real manager availability:** extend native integration coverage with a persistent failed cleanup and a capacity-blocked reservation. Assert observable downstream progress—configuration/Pod creation and an actual desired image change reaching the Pod—not merely a nil reconcile error or image getter.
3. **Deletion:** test retained `DeleteOnExpire=true` archive plus owner deletion produces no newly initiated remote-delete Job; test namespace termination with credentials present and absent. Assert native finalizer release only after durable terminalization/local handling. Preserve signer and parent deletion tests.
4. **Snapshot lifecycle:** exercise ready stop-node snapshots, invalidated native exports, time retention and count retention. Assert markers clear, the node can resume, and CSI deletion does not manufacture remote deletion proof.
5. **Capacity:** exercise thirty ordinary retained exports using realistically serialized records; then more than fifty successful export/delete/cleanup cycles with bounded live authority/catalog and an advancing watermark. Separately show that retained archives and uncertain output survive capacity pressure.
6. **Admission:** create an actual LimitRange in envtest, admit a native Pod and prove authorization succeeds. Keep negatives for lowered explicit resources and every existing archive-affecting mutation.
7. **Schema/wire:** API-negative tests for sequence above watermark, stopped cleanup reopening, timestamp reset and uncertainty erasure; exact-case decoder negatives plus valid reordered JSON.
8. **CI:** keep `TestNativeExportAttemptAPIBoundary` in `internal/controllers/chainnode`. Add a separate command inside `make test.integration`:

   ```text
   KUBEBUILDER_ASSETS=<same resolved assets>
   go test -v ./internal/controllers/chainnode \
     -run '^TestNativeExportAttemptAPIBoundary$' -count=1 -timeout <TEST_TIMEOUT>
   ```

   Run it once alongside the existing Ginkgo integration invocation. Do not pass Ginkgo flags to this package. Ensure asset resolution is nonempty and executable so this command cannot silently skip. No workflow change or production test seam is needed.

9. **Final checks:** focused owner/sibling tests, focused race coverage, integration invocation, affected vet/build, generated freshness and diff review. Preserve the prior disclosed lint baseline instead of claiming package-wide lint green.

Two existing assertions deliberately need revision: delayed POST before authorization must now retain `RemoteCleanupState=Complete`; confirmed-deleted descriptor retention must last until source absence, then permit catalog pruning. Keep their ordering, gate and watermark assertions intact.

Use the existing test-owned HTTP transport fault fixture against the actual API server. Envtest still simulates CSI binding, kubelet execution, Job completion and GC. Namespace tests prove native finalizer behavior, not full live namespace-controller completion.

After three failed attempts at the same schema or lifecycle issue, stop and reassess the design; do not apply a fourth speculative patch.

## Risks & Side Effects

Expected correction scope is roughly 15–20 authored files plus generated outputs. No dependency, image default, release, Nodezi, DB, new controller or generic retry framework changes are needed.

The remaining deliberate limits are:

- A failed cleanup requires explicit acknowledgement for local retention to proceed; it does not gain another automatic Job issuance.
- Acknowledged uncertainty continues consuming status capacity.
- Retained archives consume catalog space until their retention promise is resolved.
- Owner deletion preserves archive retention semantics but destroys the CR’s evidence.
- Admission tolerance is restricted to the resource policy above.
- Live CSI, Job-controller and real-cloud acceptance remain separate from envtest evidence.

Save this plan first, assign one fresh Sol correction writer, then freeze the complete corrected delta for the same Claude reviewer.

## Scope

Files: approximately 15–20 authored files plus generated outputs | Complexity: high. One implementation owner; focused local checks and the full matrix in CI. No merge, release, image publication, deployment, or destructive retained-evidence purge.
