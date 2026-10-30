Read-only planning is complete. **The correction is complex**, but it can remain bounded: fix protocol dispatch, native-error isolation, optional snapshot-watch startup, required resource labels, and deletion ordering; strengthen existing proofs and repair the snapshot E2E fixture. Preserve the existing terminal-hold and readiness-deferral contracts unless the independent reviewer identifies a concrete violation.

Runtime metadata confirmed once: native Codex `gpt-6-astra`, effective effort `high`, parent `f3e860be-2dc8-4331-b61e-690dff704a16`, workspace `wks_87a0ca54f8de78c1`. This is reported runtime metadata, not independent model attestation. HEAD remains `ccd1d6c8416cb5fd911dc6db4f285f0cdbc3e650`; tracked tree and index remain clean, with the same five protected untracked TEST documents.

No files, temporary artifacts, Git state, services or containers were created or changed. No tests, builds, reruns, delegation or external messages were performed.

**Root cause / goal**

Current configuration sometimes overrides the persisted export’s protocol, while a few new paths bypass the established availability and lifecycle boundaries. Correct those paths without changing immutable attempt authority, issuance witnesses, publication requirements, remote uncertainty, or retained-capacity policy.

The Allora failure is separately attributable to an invalid restore fixture and an insufficient startup assertion. It is not presently evidence of a native-export regression or baseline flakiness.

**Complete finding ledger**

“Reject” below means a recommendation for independent adjudication, not permission to dismiss or resolve the GitHub thread.

| # | Thread ID | Disposition, evidence and required outcome |
|---|---|---|
| 1 | `PRRT_kwDOOwxilM6xztej` | **Accept — Critical; shared dispatch fix.** `snapshots.go:isTarballReady` checks current `nativeConfigured` before handling a persisted legacy record. `reserveNativeAttempt` then rejects that record. Poll, complete and clean the legacy Job using its recorded destination despite switching configuration to `source-v1`. Regression: actual persisted legacy export completes without reserving native authority. |
| 2 | `PRRT_kwDOOwxilM6xzxbx` | **Recommend rejection as a contract change.** `cleanupNativeOrphans` removes terminal holders, and `cleanupNativeAttempt` explicitly releases the hold before deleting ephemeral resources. The original plan §7 and §9 permits release after confirmed terminal authority: a worker already authorized has the original clone mounted, may finish remote I/O, cannot publish after invalidation, and leaves `Required` uncertainty. The snapshot finalizer is not a lease on the mounted clone or CSI backing storage. Local resource absence still gates authority-finalizer release and CSI retention. Requiring worker absence before source-hold release strengthens a deliberately different contract; the reported continuing upload alone does not demonstrate broken provenance. Preserve the existing delayed-POST proof; obtain Opus adjudication before replying. |
| 3 | `PRRT_kwDOOwxilM6xzxby` | **Accept — Warning.** The delayed-POST subtest proves late Job creation and gate denial but omits its eventual absence. Add an actual API `IsNotFound` assertion after the second cleanup, allowing bounded observation if deletion is asynchronous. Keep replacement-source preservation and remote-state assertions. No new test function is needed. |
| 4 | `PRRT_kwDOOwxilM6xzxb0` | **Accept — Warning.** `ValidateNativeJob` validates execution and owners but not the native attribution labels used by binding, watches and cleanup selectors. Validate the **required native-label subset**, on Job and template; allow Kubernetes/admission-added labels. Audit `ValidateNativePVC` for the same required attribution contract and include its missing-label case. Cleanup Job validation must retain this check through normalization. Actual admitted/defaulted Jobs remain valid. |
| 5 | `PRRT_kwDOOwxilM6xzxb1` | **Accept — Critical.** `handleNativeSnapshotOutcome` directly returns `retainNativeSnapshot`’s error, unlike the time/count retention branches at approximately lines 412 and 510. Wrap and handle the native operation through `nativeLocal`/`handleNativeOutcome`; preserve fatal owner/context errors. Test an actual failed retention operation, retained evidence, Degraded status and continued ordinary node work. |
| 6 | `PRRT_kwDOOwxilM6xzxb3` | **Accept — Critical.** Installation prerequisites explicitly describe snapshots as optional. The unconditional watch at `controller.go:620` creates a mandatory informer. Installed controller-runtime’s `internal/source/kind.go` retries missing-kind informer acquisition and blocks synchronization. Register the watch only when REST mapping exists; distinguish `NoMatch` from other discovery failures. Prove a real manager processes an ordinary ChainNode without snapshot CRDs. Preserve normal snapshot watches when installed. |
| 7 | `PRRT_kwDOOwxilM6xzxb4` | **Recommend rejection of a new deadline in this correction.** Pending PVC deferral is real, but the implemented two-minute recovery applies to **issued resources that remain unobserved**, not an overall attempt lifetime. The read-recovery plan explicitly preserves pending PVC/initial PV/Pod behavior; round-three approval accepts unbounded readiness deferral. Extending the deadline would invalidate legitimately slow provisioning and change that accepted policy. Document the pending/readiness limit and policy-disable cancellation action more explicitly. No retry, witness reset or new deadline field. |
| 8 | `PRRT_kwDOOwxilM6xzxb7` | **Accept — documentation.** Name `persistence.restore.snapshot.name` and `persistence.restore.verification.sha256`, mapped from `status.snapshotExports[].archive`. Explain that supplying the digest is required by the descriptor-verification workflow; the general restore API still permits omission. There is no separate descriptor-mode runtime discriminator, so do not claim distinct admission enforcement. |
| 9 | `PRRT_kwDOOwxilM6xzxb_` | **No production/schema fix; clarify in the new coordinator plan.** Actual CEL at `common_types.go:1715` begins `!has(oldSelf.cleanupState) || …`, so absent→Acknowledged is already permitted. `reconcileNativeAcknowledgements` supports the stated invalidated/unresolved case; existing manual-retry acknowledgement coverage exercises it through the API. The historical plan wording is incomplete, but the requested schema relaxation is unnecessary. Keep the protected old plan unchanged; record its clarification in the new artifact and provide source evidence to Opus. |
| 10 | `PRRT_kwDOOwxilM6xzxcG` | **Recommend rejection of the proposed `PodPending` assertion.** Envtest has no kubelet; this fixture never writes the init Pod phase, so `Pending` is not a reliable replacement expectation. The substantive regression already requires the exact `PolicyOrOwnerChanged` catalog reason while the initialization Pod exists, distinguishing cancellation from missing-source invalidation. The final non-Succeeded assertion is a fixture check, not the behavioral oracle. No test should be added solely to assert kubelet behavior absent from envtest. |
| 11 | `PRRT_kwDOOwxilM6xzxcM` | **Decline optional optimization.** `DeleteResult` intentionally generates exact split keys lazily, bounds concurrent workers, cancels and joins them, and never lists by prefix. Existing HTTP coverage independently checks exact keys and unrelated-object preservation. Batch deletion could reduce requests, but no measured problem or correctness defect is supplied. Do not route through the legacy listing-based deletion path. Keep batching out of this correctness revision; seek independent agreement before resolving. |
| 12 | `PRRT_kwDOOwxilM6xzxcP` | **Accept — Warning; pair with #13.** `Reconcile` already runs recovery before either deletion branch. Both `finalizeResources` and `finalizeTerminatingNamespace` repeat it. Remove the duplicate pairs from both helpers while preserving the single early recovery boundary. Verify through the production reconcile entry, not a call-count-only test. |
| 13 | `PRRT_kwDOOwxilM6xzxcR` | **Accept — Critical; cover both finalizers.** Both helpers return on `AuthorityFinalizer` before quiescing node/signing workloads. A protected native resource or local API failure can therefore keep the signing workload alive during deletion. Quiesce first; retain the native gate before durable cleanup/finalizer release. `cosmosigner.FinalizeOwner` also performs durable-state cleanup, so use existing `QuiesceOwner` before the gate and retain `FinalizeOwner` after it, rather than accidentally moving signer-state deletion ahead of the barrier. Namespace termination should similarly quiesce before gating and release state finalizers afterward. |
| 14 | `PRRT_kwDOOwxilM6xzxcU` | **Accept the documentation alternative; retain RBAC scope.** `role.yaml` is a **ClusterRole**; manager configuration watches across namespaces. `ensureNativeIdentity` creates a namespaced Role/RoleBinding per authority owner, granting only GET on that owner’s exact ChainNode name. The operator therefore needs these namespaced-resource permissions across managed namespaces. The rule does not grant ClusterRole/ClusterRoleBinding mutation, `bind`, or `escalate`. Document the deliberate scope; do not introduce a namespace allowlist or hand-edit generated RBAC. Preserve existing impersonated-worker RBAC acceptance. |
| 15 | `PRRT_kwDOOwxilM6xzxcX` | **Accept with #1/#17 — dispatch correction.** `exportTarball` dispatches solely on current configuration. With a persisted native record and legacy configuration, it can reach `tarballProviderForExport`, whose guard returns a plain error. `handleNativeSnapshotOutcome` does **not** absorb arbitrary plain errors. Route existing native records through their dedicated lifecycle before legacy provider construction, including marker-removal/manual-retry cases. Preserve the guard as defense in depth. |
| 16 | `PRRT_kwDOOwxilM6xzxcb` | **Accept — small diagnostic fix.** `nativeExportRequests` discards the `nativeHolders` decode error. Log it once at this non-returning boundary with snapshot namespace/name, then preserve any already-derived label request. Do not log the raw annotation or add logging-only tests. |
| 17 | `PRRT_kwDOOwxilM6xzxcf` | **Accept — duplicate of #1.** Keep the reservation rejection: it correctly refuses conversion of an existing legacy record. Fix its callers’ protocol dispatch instead. One regression and one implementation concern cover both IDs. |
| 18 | `PRRT_kwDOOwxilM6xzxcj` | **Accept — documentation/generated output.** Add a concise `Protocol` field comment describing omitted/`legacy` behavior and `source-v1`’s S3 Secret-backed restriction. Regenerate both affected CRDs and reference documentation using existing tools. No YAML hand edits or default-image changes. |
| 19 | `PRRT_kwDOOwxilM6xzxcm` | **Accept — Warning.** The generic `"authority"` substring can match setup failures. Add expected denial per table row and record that the authority endpoint was reached. Normal denials should match `native authority denied`; missing owner should prove the expected GET/NotFound outcome. Preserve zero store requests and pre-filesystem denial. |
| 20 | `PRRT_kwDOOwxilM6xzxcp` | **Accept — test correctness.** Replace handler-goroutine `require.Empty` with nonfatal reporting or captured request evidence asserted by the test goroutine. Return an explicit HTTP failure when appropriate. Keep the uncached-read check; do not test the assertion helper itself. |
| 21 | `PRRT_kwDOOwxilM6xzxcs` | **Accept — documentation.** Replace internal roadmap labels with plain language about provider expansion and restore consumers. Preserve the explicit absence of a retained-archive release path. |
| 22 | `PRRT_kwDOOwxilM6xzxcw` | **Valid documentation defect, protected-file exception.** The historical locations drifted, and colon-suffixed relative paths are not portable GitHub file-line anchors. Existing plans must remain unchanged under this assignment. Put current function/file references in the new coordinator artifact, preferably stable file links plus function names. Do not authorize the writer to modify the old plan. Root should explain this treatment rather than silently dismiss it. |
| 23 | `PRRT_kwDOOwxilM6xzxc4` | **Accept a targeted extension — Warning.** Cleanup’s separate normalization is a distinct security boundary; current shared coverage checks resources, while tampering covers upload only. Extend the existing table with cleanup-relevant command/args, working-directory, identity, API mount/volume and sidecar cases. Do **not** blindly replay the upload PVC mutation: cleanup has no data PVC, so that row would dereference a different volume shape. Keep valid cleanup and admission-resource positives. |
| 24 | `PRRT_kwDOOwxilM6xzxc5` | **Accept — documentation.** Document `--pod-uid` and its `EXPORT_POD_UID` default, matching `newNativeCommand`. This identifies an already-selected worker; it does not grant authority to arbitrary diagnostic execution. |
| 25 | `PRRT_kwDOOwxilM6xzxc9` | **Accept narrowly, based on routing policy rather than missing unit tests.** Cover owner-name+UID attribution, same-namespace holders, cross-namespace rejection, malformed JSON/holder entries, and non-snapshot objects at the mapper/event boundary. This protects which authority receives recovery events. Avoid a separate inventory of trivial predicate booleans. Pair with real-manager watch acceptance; no exported test seam or log-string assertions. |

**Bounded implementation flow**

1. **Persisted protocol first.** Resolve the existing record before choosing the configured protocol in both start and polling paths. Existing legacy records retain recorded destination/completion/cleanup. Existing native records never reach legacy providers. Only eligible new work follows current configuration. Manual native retry continues to allocate a new sequence/UUID; disabled native policy must not silently start legacy work over native evidence.

2. **Keep native failures local.** Apply the existing typed error boundary to deferred retention. Ordinary snapshot/configuration/signing work continues; owner identity/read failures and canceled contexts remain fatal.

3. **Make snapshot watch optional.** Use the manager’s REST mapper during setup, skipping only a genuine absent snapshot kind. Do not add a dynamic controller framework, feature flag or dependency. Document restart after installing optional snapshot CRDs if the watch was omitted at startup.

4. **Validate required attribution.** Check native label values as a subset, preserving Kubernetes-generated Job labels and admitted execution defaults. Keep exact owner/UID, execution shape, mounted gate and cleanup checks.

5. **Quiesce during deletion.** Keep one early native-recovery pass. Both deletion helpers stop signing workloads even while native local cleanup remains pending, then retain the existing native and durable-resource barriers. Preserve reservation-owner and orphaned-child checks.

6. **Repair focused tests and documentation.** Reuse current tables and API fixtures. Keep old plans immutable; root saves this complete plan and its historical clarifications before assigning the fresh Sol writer.

No changes are recommended to the authority schema, cleanup issuance model, native hold-release ordering, pending-readiness lifetime, or exact S3 deletion implementation.

**Allora CI causality and proposed correction**

The saved log establishes:

- Source `e2e-allora-chainnode-vdrd9` initialized genesis, ran, and produced ready snapshot `…20261030005815`.
- The test waited for source height above five at `00:58:41`, but selected a snapshot captured earlier.
- Restored `e2e-allora-chainnode-m7c6j` provisioned its PVC, then repeatedly failed `ensureGenesis` at `00:59:24`, `01:00:05` and `01:01:27`.
- Diagnostics show empty phase/image, height one and no running restored Pod. The five-minute height timeout is the downstream symptom.

The source trace explains this:

- `BuildChainNode` initializes genesis into a ConfigMap.
- `pod.go` mounts that ConfigMap at `/home/app/config/genesis.json`; it is not part of the data-volume snapshot.
- The restore test deletes the source, removes validator initialization, and sets only `UseDataVolume=true` plus `ChainID`.
- `pvc.go` creates the restored PVC with data-initialized/height metadata, not the genesis-downloaded marker.
- `ensureGenesis` therefore falls through to `getGenesis`, whose URL/RPC/ConfigMap cases are all absent, producing the exact logged error.

These paths and the test are unchanged against `ec70a11`. Native export is not configured in this test.

There is also a false-positive path: `WaitForChainNodeHeight` reads `status.latestHeight`, which `pvc.go` seeds from the snapshot annotation. A snapshot recorded above one can satisfy the current assertion before any restored process starts. This explains why passing sibling shards do not prove the fixture is correct; it does not establish their actual timelines.

Recommended minimal repair in `test/e2e/chainnode_snapshot_test.go`:

- Before deleting the source, copy its actual genesis ConfigMap data into a distinct namespace-scoped fixture ConfigMap without the source owner reference.
- Configure the restored node to use that ConfigMap; retain `Validator=nil`.
- Wait for source deletion using `IsNotFound`, rather than treating any GET error as deletion.
- Require the restored Pod to become ready and read its live RPC height. The existing `liveValidatorHeight` helper uses Kubernetes Pod proxy and can serve this observation despite its name.
- Assert restored data is observable at a positive height consistent with the selected snapshot. Do not demand new block production after deleting the sole validator, and do not use seeded CR status as startup proof.
- Keep the existing timeout budget.

This is a source-backed **fixture correction**, not a proposed change to production genesis handling. No local execution has proved the repaired fixture yet. Do not rerun the failed job merely to seek green; validate the corrected fixture in the next full CI revision.

**Focused TDD and acceptance**

| Boundary | Required proof |
|---|---|
| Protocol transition | Actual legacy Job/status survives a configuration switch, reaches completion and cleanup, and creates no native reservation. Native records under changed configuration never create/adopt a legacy Job. Preserve new native manual-retry behavior. |
| Native retention availability | Inject a real API transport failure at the retention operation. Assert it fired, evidence remains, Degraded is visible, and ordinary Pod/configuration progress still occurs. Retain owner/context fatal controls. |
| Deletion ordering | Through `Reconcile` or a real manager, keep an exact native resource pending with a protection finalizer. Assert node/signing teardown proceeds while native/durable finalizers remain. Cover normal owner deletion and namespace termination. Release only the test-owned blocker and prove eventual completion. |
| Labels/admission | Real API-admitted Job/template labels remain accepted. Missing/replaced required labels are denied before authorization. Additional Kubernetes/admission labels remain accepted. Exercise cleanup normalization and PVC attribution where validated. |
| Optional CRD | Start a real manager with only required Cosmopilot CRDs and no snapshot CRDs; observe ordinary ChainNode reconciliation creating its init Pod. Existing snapshot-installed manager acceptance remains positive. |
| Event routing | Small independent table for namespaced attribution and malformed-holder handling; manager proof covers actual watch delivery. |
| Existing race/gate tests | Add late Job absence, precise CLI denial and handler-safe assertions without duplicating lifecycle fixtures. |
| Allora fixture | Full CI exercises actual CSI restore, explicit genesis source, Pod readiness and live RPC observation. No local E2E or timeout expansion. |

Record behavioral RED before each production fix. Compile/setup failures do not count. For assertion-only improvements, accurately report strengthening existing coverage rather than claiming a newly reproduced product defect.

A no-snapshot-CRD test needs its own envtest environment. Keep it sequential and include it explicitly in the existing owning-API CI command, alongside `TestNativeExportAttemptAPIBoundary`; otherwise unit runs without verified assets may silently skip it.

**File ownership**

One fresh Sol writer should own this coupled correction.

- Production: `internal/controllers/chainnode/snapshots.go`, `controller.go`, `resource_cleanup.go`, `snapshot_export_watch.go`; `internal/datasnapshot/export_attempt.go`.
- API documentation: `api/v1/common_types.go`, limited to the protocol field comment.
- Existing tests: `snapshot_export_attempts_test.go`, `snapshot_export_review_fixes_test.go`, `internal/datasnapshot/export_attempt_test.go`, `cmd/dataexporter/cmd/native_upload_test.go`.
- New focused tests if needed: `internal/controllers/chainnode/snapshot_export_watch_test.go` and a manager-without-snapshot-CRD test file.
- CI selection: `Makefile`, only if needed to include that separate actual-API test in the existing invocation.
- E2E fixture: `test/e2e/chainnode_snapshot_test.go`.
- Operator documentation: `docs/docs/usage/persistence-and-backup.md`, `docs/docs/usage/restoring-from-snapshot.md`, `docs/docs/reference/cli.md`.
- Generated outputs: both ChainNode/ChainNodeSet CRDs and `docs/docs/reference/crds/crds.md`.
- Coordinator only: the new plan/ledger artifact and subsequent freeze/review artifacts.

Expected scope: approximately **17 authored files, three generated outputs and one coordinator plan**, roughly **300–600 changed lines**, high complexity. Final size depends on reusing the existing API fixtures.

Keep `go.mod`, `go.sum`, all five TEST documents, existing plans, image defaults and Nodezi unchanged. `snapshot_export_attempts.go`, `snapshot_export_attempt_cleanup.go`, protocol/worker production code and S3 deletion should remain unchanged unless adjudication establishes an additional concrete requirement.

**Generated freshness, resource guards and review gates**

- Inventory/hash protected files and the writer’s ownership set before editing.
- Use existing verified generator binaries only. Regenerate through the repository’s manifest/docs recipes; inspect every output. Deepcopy and RBAC should remain byte-identical unless an actual corresponding source change justifies otherwise.
- Format changed Go files only; run focused tests, affected build/vet, added-line static checks and `git diff --check`.
- Run the owning API boundary once on the final revision with explicit executable `KUBEBUILDER_ASSETS`. Preserve existing schema, impersonated RBAC, admission, read-fault, publication, retention, capacity and late-resource tests.
- Register envtest stop, manager cancellation/join and test-owned HTTP shutdown before starting work. Track exact spawned server processes and temporary directories; verify absence afterward. Preserve pre-existing resources.
- No local containers are needed. If scope later requires them, inventory first and remove only task-created containers, anonymous volumes and networks on every exit path.
- Preserve the disclosed baseline lint failures; do not call package-wide lint green without evidence.
- Root obtains retained Opus adjudication on #2, #7, #9, #10, #11 and #22, plus the contract-sensitive interpretations in #8, #13 and #25, before replies/resolutions.
- After implementation and focused verification, freeze the complete delta for independent review. Run the next full CI revision once, including all E2E shards. Existing push CI and five passing shards are useful evidence, but the current full E2E run remains failed.

The main preservation risks are accidental legacy/native conversion, overstrict label equality, moving durable signer cleanup ahead of safety barriers, and silently expanding a two-minute missing-resource recovery window into a general attempt deadline.

## Authoritative independent-review amendments

The independent Opus review approves this correction subject to R1–R6 below. These amendments override conflicting recommendations in the proposed ledger and implementation flow above. The original proposed plan and full ruling remain separate delivery evidence.

1. **R1 — signer ordering:** keep `cosmosigner.FinalizeOwner` whole, including attribution before quiescing and its existing explicit state policy. Place the native authority gate after this call. Do not call bare `QuiesceOwner` before attribution. Namespace termination quiesces the node and calls `QuiesceOwnerForNamespaceTermination` before the native gate, then removes parent finalizers. Preserve effective deletion policy and orphaned-recorded-child rejection ahead of these operations. Tests must preserve attribution-before-StatefulSet-deletion, rather than assuming every signer-state finalizer is governed by the independent native snapshot barrier.
2. **R2 — missing snapshot kind:** optional watch setup is insufficient by itself. The owner-NotFound orphan-recovery path also lists VolumeSnapshots. Treat a genuine snapshot-kind NoMatch there as no readable source holders, while continuing applicable Job/PVC cleanup and preserving other discovery/read errors. Cover ordinary creation and deletion through a real manager without snapshot CRDs. Document restarting the operator after installing the optional CRDs to register their watches.
3. **R3 — stalled provisioning visibility:** preserve unbounded legitimate readiness/provisioning and one-use issuance. Once the existing two-minute window since CreatedAt has elapsed, a still-pending issued resource must produce a bounded local `RecoveryDeferred` outcome so Degraded stays visible. Invalidate nothing, add no deadline/reset/retry fields, and document source pinning plus the remedies of fixing storage or disabling the policy. This explicitly permits narrow changes to the existing binding function in `snapshot_export_attempts.go`.
4. **R4 — restore mapping:** document objectKey to snapshot.name; destination provider, bucket, region, endpoint and forcePathStyle to the corresponding persistence.restore.snapshot fields; sha256 to verification.sha256. Credential selection is separate: readIdentity is provenance, not automatic restore authorization. Requiring SHA-256 is this descriptor workflow's rule; the generic restore API does not enforce a descriptor-specific mode.
5. **R5 — fixture-only phase assertion:** API servers do default new Pods to Pending, even without kubelet. The proposed rejection's contrary factual explanation is incorrect. Remove the redundant non-Succeeded fixture assertion from the existing policy-disabled integration test rather than replacing it with another fixture-decided phase assertion. Preserve its exact PolicyOrOwnerChanged reason and init-Pod existence, which enforce production behavior. Include `test/integration/snapshot_export_attempt_test.go` in ownership for this test-audit cleanup.
6. **R6 — real restore proof:** choose a ready snapshot for the source whose creationTimestamp follows observed source progress above five; promptly restore it, preserving the existing time budget. The height annotation is advisory, not an exact data-height claim. Copy actual source genesis into a distinct ownerless fixture ConfigMap before deleting the source and configure that explicit source on restore. Require real container startup and positive live Pod-proxy RPC height; do not use seeded CR status or demand new block production after sole-validator removal. Prefer existing readiness behavior; if it depends on unavailable peers, use a started container plus the existing RPC health/startup boundary and live height. Source deletion must be confirmed specifically by NotFound.

These amendments additionally authorize the existing `snapshot_export_attempt_cleanup.go` only for missing-kind orphan handling, and the existing integration test for R5. They do not authorize changing source-hold release after confirmed terminal state, immutable authority, remote uncertainty, exact S3 deletion, cleanup issuance, or schema fields. All other accepted Critical and Warning outcomes and required-label subset validation remain mandatory.

The coordinator owns minimal errata in the two older plans: clarify absent-to-stopped cleanup initialization without changing CEL, and replace colon-line Markdown links with plain source-file links and named functions. Their before/after raw-byte hashes are recorded in delivery evidence. The writer must preserve these coordinator-owned plan bytes.

## Actual make invocation refinement

The installed macOS `/usr/bin/make` is GNU make 3.81 and ignores `.SHELLFLAGS`. The previously recorded shell-extracted guard proof therefore does not establish fail-fast behavior of the actual `test.integration` recipe. An invalid or empty asset resolver must stop before either Go test invocation, and failure of the owning API test must stop before the later integration command.

This authorizes minimal explicit failure handling within the existing recipe, beyond its owning-test selector change. Keep the current shell/tool configuration, asset identity checks and exact-once owner selection. Do not install another make or change global configuration. Capture RED/GREEN using the actual installed make with task-owned resolver/fake-Go fixtures and invocation counts, including first-owner failure. Preserve the predecessor proof with its narrower extracted-shell limitation, and distinguish actual recipe validation from real API execution.
