# Native export provenance: read recovery and manual retry

This plan is saved before correction implementation. It refines the earlier provenance and availability plans for accepted round-two W-A, W-B and W-C findings. Public contracts, the original plans, and the frozen prior handoff remain unchanged.

The bounded correction should touch **five existing files**, with no public API, schema, dependency, worker, or issuance-contract changes. The coordinator must save this plan before assigning the fresh Sol writer.

Runtime verified once: native Codex `gpt-6-astra`, effective effort `high`; parent `f3e860be-2dc8-4331-b61e-690dff704a16`; workspace `wks_87a0ca54f8de78c1`. Paseo exposes configuration/runtime metadata, not independent model attestation. Its reported working directory is the shared Voluzi root; every shell invocation explicitly used `/Users/admin/projects/voluzi/cosmopilot`.

Branch remains `feat/native-export-provenance`, HEAD `ec70a11e2e195b526bd61f5fb0c4d38bf7c39727`, with an empty index. The raw correction-manifest hash matches `ce9905ee6a2f4c017eecff6c2fa4e106055c37df157d99add53b26493824a6c0`; all five proposed files match their frozen manifest hashes.

**Root cause and finding ledger**

| Finding | Source-backed cause | Disposition |
|---|---|---|
| W-A | [`stopNativeCleanup`](../../internal/controllers/chainnode/snapshot_export_attempt_cleanup.go:817) sets `Required` only when an issuer exists. A definitive credential loss before issuance therefore records `Failed` alongside `Complete`. | Fix. Every `Failed` cleanup must retain `Required`. |
| W-B | [`failNativeSnapshotMarker`](../../internal/controllers/chainnode/snapshot_export_attempt_cleanup.go:801) rewrites every matching snapshot except an already-failed marker. Recovery processes retained invalidated attempts before the normal snapshot retry loop. | Fix. Repair only the exact current native export’s still-`true` marker. |
| W-C | Recovery, binding and publication turn failed reads into invalidation. [`nativeSource`](../../internal/controllers/chainnode/snapshot_export_attempts.go:131) also combines unavailable readiness with identity mismatch; credential classification treats GET `Forbidden` as lost identity. | Fix through a private definitive-identity error contract and explicit read classification. |
| Secret GET `Forbidden` | It proves the controller could not read the Secret, not that the original Secret disappeared or changed UID. | Accept this suggestion within W-C. Defer native work without terminalizing it. |
| Secret `NotFound` | A fresh exact read establishes absence of a frozen identity. Recreating the Secret produces a different UID. | Keep definitive loss. Do not wait for a replacement identity. |
| Cleanup POST `Forbidden`/quota | The issuance witness was already committed. A definitive rejection cannot authorize another POST under the one-issuance contract. | Keep `Failed/CleanupCreateRejected`; reject automatic reissuance. |
| Conflict-induced Degraded flap | Separate from incorrect terminalization; changing condition handling would broaden this correction. | Defer. Preserve current status-write/readback behavior. |
| Retained archive capacity | Retained descriptors and acknowledged uncertainty remain bounded but have no release mechanism. | Accepted limitation. Require a retention/release decision before P2 or Nodezi C; add no purge path now. |

C1–C3, W1–W5 and N1–N3 remain preservation requirements, with W-B correcting the remaining W1 retry defect.

**Implementation contract**

1. **W-A: truthful failed-cleanup state**

   In `stopNativeCleanup`, use:

   ```go
   current.CleanupIssuer != "" || state == "Failed"
   ```

   as the condition for setting `RemoteCleanupState = "Required"`.

   Copy that state to the catalog and retain the existing bounded “deletion remains unproven” message. `nativeSave` already synchronizes `SnapshotExportCleanup`; no separate condition writer is needed.

   Preserve `Abandoned` plus `Complete` for owner deletion when no remote cleanup was issued and no unresolved output existed. Do not introduce the optional catalog-Deleting abandonment refinement in this correction.

   Preserve the descriptor, completed envelope, upload terminal state, immutable identities, recipe, authorization timestamp and issuance witnesses. Do not manufacture an issuer or `CleanupComplete`.

2. **W-B: repair only the current attempt’s abandoned progress marker**

   Change the private helper signature to receive the owner and expected attempt:

   ```go
   failNativeSnapshotMarker(ctx, c, a, recipe) error
   ```

   Update its two callers in `cleanupNativeAttempt` and `compactNativeAttempts`.

   Apply these checks in order:

   - Read the source snapshot uncached.
   - Return without mutation for NotFound, a different UID, a deleting snapshot, or any marker other than literal `"true"`.
   - **After reading the snapshot**, read the owner through `nativeRead`.
   - Require the expected sequence/UUID still to identify an `Invalidated` attempt.
   - Use `snapshotExportFor(fresh, snapshot)` and require native provenance, exact snapshot UID, and the expected attempt/export ID.
   - Change only the exporting annotation to `failed`, using the snapshot’s fetched resource version.

   The read order matters: a retry publishes its new catalog record before setting its marker. Reading the snapshot first and the owner second prevents an old catalog view from being paired with a new retry’s marker. A subsequent marker change must cause the stale snapshot update to conflict.

   Preserve absent, failed, finished, uploaded and other markers. Missing catalog/attempt or a newer matching native export means no repair. Owner read/UID failures remain fatal. Other snapshot read/update errors remain deferred through the existing native boundary.

   Repeated cleanup may still handle old attributable resources and holds. It must not reopen or rewrite the old attempt’s terminal evidence. Keep `snapshotExportFor`’s existing newest-native/first-legacy semantics.

3. **W-C: distinguish failed identity proof from unavailable evidence**

   Replace the narrow private `nativeIdentityUnavailableError` with one private definitive-proof type:

   ```go
   type nativeIdentityError struct{ error }

   func (e *nativeIdentityError) Unwrap() error { return e.error }
   ```

   No exported type, new status field, generic retry framework or string-based classification is needed.

   Its contract is strict: construct it only for a definitive absence of an already-frozen identity, deletion, or an observed UID/binding/hold/execution mismatch. Ordinary API failures must retain their causes and must never acquire this type.

   `nativeSource` and `nativeCredentialIdentity` keep their existing signatures. Their callers use `errors.As` to decide whether invalidation or stopped cleanup is justified. Operational errors enter existing `nativeLocal` handling, normally as `RecoveryDeferred`; credential operations may retain `ExecutionIdentityUnavailable`.

   Preserve existing terminal reasons such as `SourceBindingChanged`, `ExecutionIdentityChanged`, `JobMissing`, `PVCMissing`, `PVMissing`, and `PodMissing`. Conditions remain bounded by `nativeCondition`; no new reason vocabulary is necessary.

   Apply the following classification:

   | Observation | Required behavior |
   |---|---|
   | Owner GET failure, missing APIReader, owner UID mismatch | Fatal `nativeOwnerError`; no local continuation. |
   | Error matching `context.Canceled` or `context.DeadlineExceeded` | Fatal; preserve wrapping and `errors.Is`. |
   | Non-owner GET/LIST 429, 5xx, server timeout, transport failure, Forbidden or Unauthorized | Local operational error; no invalidation, cleanup stop, publication or dependent issuance. |
   | Frozen snapshot/content/Secret/SA NotFound, deletion or UID replacement | Definitive identity loss. |
   | Present conflicting snapshot/content name, restore size, reverse reference, driver or required hold | Definitive mismatch. |
   | Snapshot/content status or readiness evidence temporarily incomplete, without a contradictory identity/binding/hold | Local deferral; do not authorize or publish. |
   | Initially unobserved issued Job/PVC | Preserve the existing bounded recovery window measured from `CreatedAt`; never reissue. |
   | Bound Job/PVC/PV/selected Pod disappears or is replaced | Definitive loss; retain postauthorization uncertainty. |
   | PVC awaiting initial binding, initial PV not yet observable, no selected Pod, or incomplete Job | Preserve the existing pending behavior; no authorization. |
   | Observed invalid Job/PVC/Pod shape, duplicate selected workers, or invalid established PV binding | Preserve definitive invalidation. |

   Split `nativeSource`’s combined checks so readiness absence cannot conceal an observed identity mismatch. Check available identity/binding fields and required holds before returning a readiness deferral. Content UID/deletion/reverse-binding checks must precede its readiness-only return.

   For publication, handle Job/PVC/PV/Pod GET errors explicitly: NotFound invalidates; other errors defer. Before the PV GET, detect an empty or conflicting PVC volume name against the frozen PV reference as a binding mismatch, rather than allowing an invalid GET to become indefinite operational deferral.

   Preserve `validateNativePV`’s established-binding checks. Initial PVC/PV readiness remains pending before selection; a successfully observed broken established binding remains invalid.

   Audit and correct **both** the early recovery proof and the repeated proofs inside `bindNativeResources` and `publishNativeArchive`. An earlier successful read does not classify a later failed read.

   In `holdNativeSource`, retain ambiguous-write readback semantics: only exact confirmation permits advancement. Ensure a canceled/deadline readback is not hidden behind the earlier write error. Source checks in hold/issuance must deny their dependent action on every failure; they must never consume a witness or POST following an unconfirmed proof.

   In cleanup reconciliation, only the definitive private identity type triggers `Failed/ExecutionIdentityUnavailable`. Secret/SA read errors otherwise defer without consuming a previously absent cleanup issuer or resetting an existing one.

**Sibling audit and preserved boundaries**

The following paths were traced and should retain their existing semantics:

- `nativeRead`, `nativeSave`, `nativeTransition`: uncached owner identity, CAS, condition-aware uncertain-write confirmation, and metadata/spec-guarded resource-version refresh.
- Reservation and execution-identity creation: failures before reservation produce local deferral, without watermark advancement or partial authority.
- `releaseNativeHold`, marker repair, local deletion and absence checks: NotFound has operation-specific meaning; other read errors cannot prove absence.
- `nativeDeletionRequired` and `nativeSourceGone`: source NotFound may establish expiry/removal; other errors cannot establish deletion or permit compaction.
- Orphan recovery: fresh authority read per candidate, fatal non-NotFound owner failures, exact UID/RV deletion and preservation of unrelated holders.
- Both finalization entry points: local absence barriers remain mandatory; remote credentials remain unnecessary during owner/namespace termination.
- `Authorize`, `AuthorizeCleanup` and native CLI: unchanged selected-worker and stopped-authority gates before filesystem/S3 work. This correction does not add worker-side source-resource reads or revoke work that already passed authorization.
- Native/legacy dispatch and retention: old invalidated evidence stays separate from a newly reserved attempt; neither acknowledgement nor CSI retention becomes deletion proof.
- Capacity, strict decoding, CEL, admission normalization, watches and 30-second fallback: unchanged.

**Test-first acceptance**

Use the existing actual API boundary and test-owned HTTP transport. Add no fake Kubernetes client, fake cloud success, exported production seam, source overlay or test hook. Record behavioral RED separately from compilation/setup failures.

| Regression | Required observable proof |
|---|---|
| W-A: credential loss before cleanup issuance | With a live owner, valid successful descriptor and frozen `DeleteOnExpire=true`, expire the source through native retention. Delete the original Secret or replace its UID before cleanup issuance. Assert `Failed/ExecutionIdentityUnavailable`, `Required` on attempt/catalog, bounded unproven-deletion message, `SnapshotExportCleanup=True`, no cleanup POST/issuer, unchanged descriptor/envelope/witnesses. |
| W-A control | Retain the existing owner/namespace deletion matrix with credentials present/absent and prior failure. Never-issued owner-deletion abandonment remains `Complete` and emits no remote-delete POST. |
| W-B retry | Use an actual ready snapshot/content pair with correct reverse binding. Retain a postauthorization invalidated attempt. Prove current `true` repairs to `failed`; remove the annotation through the API; run recovery **before** `ensureVolumeSnapshots`; assert a new sequence, UUID, catalog record and object basename. |
| W-B acknowledgement | Run the retry scenario both before and after exact acknowledgement. Old uncertainty remains `Required`; acknowledgement does not reset its terminal state, recipe or witnesses. |
| W-B old-attempt isolation | Recover the old attempt while the new attempt’s marker is `true`; it must remain `true`. Preserve other markers, a replacement snapshot UID and a deleting snapshot. Exercise a delayed marker update against an intervening API annotation change and require RV conflict rather than clobbering. |
| W-C completed upload | Extend the existing concrete S3 byte/hash fixture before descriptor publication. Inject failures on snapshot, content, Secret, SA, Job, PVC and PV/Pod GETs. Assert the attempt remains `Uploading`, its witness/identities/envelope resources and hold survive, no descriptor appears prematurely, and no additional resource POST occurs. |
| W-C recovery after fault | Remove the fault and publish the **same** attempt and key, with the independently measured original compressed-byte digest and size. Require one Job POST and one PVC POST for that attempt. |
| W-C repeated proof reads | Inject after the early proof succeeded, targeting the later source/credential reads in binding and publication. Assert the intended request was reached. |
| W-C readiness controls | Real API status changes that temporarily remove readiness must defer, while otherwise-valid source/content UID, reverse-binding, hold, credential and established execution-binding negatives still invalidate. Restore readiness and prove progress without a new attempt. |
| W-C fatal controls | Actual owner-read denial and UID replacement remain fatal. Canceled/deadline contexts remain fatal with no terminal mutation. Distinguish a server timeout response from a canceled reconcile context. |
| Cleanup operational reads | Secret/SA GET Forbidden and transient errors do not stop cleanup or consume a new issuer. After recovery, the existing path proceeds once. A definitive rejected cleanup POST remains stopped without reissuance. |
| Core progress | Extend the existing real-manager availability fixture with a persistent, narrowly targeted native read fault. Observe ConfigMap/Pod creation and a desired image change while the same native attempt stays nonterminal and Degraded is visible. |

Use table rows for equivalent resource-read failures. Cover 429, 500/503 and server timeout responses, plus a transport failure; include Secret/SA Forbidden explicitly. Avoid an unnecessary Cartesian product when one row proves the shared classification and resource rows prove each distinct call site.

For deterministic wire faults, target exact method/resource paths and selected read occurrences; leave discovery, unrelated owners and ordinary requests delegated to the real API. Assert each fault fired. Client-go’s installed retry code distinguishes responses with `Retry-After`; account for transport retries so a fault cannot be silently consumed before reaching production.

The existing seeded terminal fixtures are appropriate for cleanup-state assertions. They are insufficient by themselves for W-B’s retry or W-C’s successful publication proof: those need valid source bindings and the existing concrete upload fixture.

**Files and scope**

| File | Change |
|---|---|
| [`snapshot_export_attempts.go`](../../internal/controllers/chainnode/snapshot_export_attempts.go) | Private definitive-identity contract; source/credential classification; binding/publication read handling; cancellation-preserving hold readback. |
| [`snapshot_export_attempt_cleanup.go`](../../internal/controllers/chainnode/snapshot_export_attempt_cleanup.go) | W-A status correction; guarded marker repair and callers; recovery and cleanup identity discrimination. |
| [`snapshot_export_attempts_test.go`](../../internal/controllers/chainnode/snapshot_export_attempts_test.go) | Actual wire faults and same-attempt publication proof using concrete stored bytes. |
| [`snapshot_export_review_fixes_test.go`](../../internal/controllers/chainnode/snapshot_export_review_fixes_test.go) | Pre-issuance cleanup failure, manual retry/marker isolation, readiness/identity cases and manager availability extension. |
| [`persistence-and-backup.md`](../../docs/docs/usage/persistence-and-backup.md) | Explain annotation-removal retry for a still-existing eligible snapshot, unchanged old evidence, operational-read deferral, and retained-capacity follow-up. |

Estimated scope: approximately 150–250 production lines and 350–600 test/support lines, plus a short documentation change; high-risk lifecycle logic with a bounded five-file surface. These are planning estimates, not quotas.

No changes to API types, generated artifacts, Makefile/CI, workers, resource rendering, dependencies, image pins, protected TEST documents or either existing root plan are expected. If implementation needs another public contract, persistent state, reissuance mechanism or broad restructuring, return to the coordinator with the minimal proposed contract before expanding.

**Verification and handoff**

The writer should:

1. Capture the five-file starting bytes and protected-state baseline before editing. Save each targeted RED result, then apply its bounded correction.
2. Run the new focused API subtests during development with explicit, verified envtest assets.
3. After the final behavior edit, run the complete owning `TestNativeExportAttemptAPIBoundary` once, preserving its existing provenance, capacity, termination, late-PVC, admission, condition-readback and manager proofs.
4. Run the affected owner `Finaliz|Cleanup|Snapshot|Tarball|Requeue` selection, focused native protocol/CLI/resource-validation tests, `go build ./cmd/...`, and affected-package vet. Use no broad local suite, race matrix or E2E run.
5. Leave the existing CI owner-API invocation intact. Full CI runs once per pushed revision. Do not repeat it locally before review.
6. Check formatting, incremental diff, added-line static findings and unchanged generated/protected bytes. Preserve the disclosed 28 baseline lint findings; do not report package-wide lint as green.
7. Freeze the complete incremental delta, full changed test bodies, exact manifests, RED/GREEN logs, test limits and resource cleanup evidence for the retained Opus reviewer.

Before any envtest run, inventory pre-existing relevant processes and directories. Register cleanup before startup; track exact task-owned descendants and data/certificate directories. Cancel and join fixtures/managers, stop owned servers and verify their absence on success, failure, timeout and interruption. Preserve unrelated processes and pre-existing directories. No containers are needed; if later authorized, their inventory and exact cleanup obligations apply independently.

**Risks and proof limits**

The principal risks are misclassifying a real identity loss as a retryable read, swallowing owner/context failure, racing marker repair against a new attempt, and allowing one test’s fault selector to affect unrelated reconciliation. The classification table, positive controls, duplicate-read faults and actual API CAS tests address those risks.

The planner made no source/artifact writes, started no tests, builds, envtest, services or containers, and created no agents. I read both plans, both handoffs, both review rounds and supplement, the complete two affected owner API test files, and the relevant callers, worker/resource contracts and CI invocation. This was a targeted source audit, not an exhaustive filesystem or whole-repository review; the parent’s broader frozen/protected inventory remains separate evidence.

Future test results will remain bounded evidence: envtest simulates CSI binding, kubelet/Job completion and GC; the manager fixture primes configuration inputs. HTTP storage tests do not prove cloud IAM or physical mounts. Fresh absence reads do not exclude every later delayed POST. Already-authorized workers may finish remote I/O, and blocked filesystem calls may delay cancellation.

Retained archives with default `deleteOnExpire=false` can eventually exhaust capacity. A release/retention decision is required before P2 or Nodezi C. Nodezi remains untouched and pinned to stable Cosmopilot 5.3.1. This plan authorizes no merge, release, image publication or deployment.
