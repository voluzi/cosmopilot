# Completed archive upload results

## Root cause and goal

The exporter returns only an error, so neither callers nor the native controller receive the exact completed object key, compressed-byte SHA-256 or size. ETags, cloud CRCs and the native export identity hash do not establish archive byte identity. This is the first independent prerequisite for Nodezi's safe exported-archive restore; it does not establish snapshot/worker provenance or enable restore.

Start from `main` commit `c987b34481b334005e9abae30e61e1cfdf0db95c` on `feat/export-archive-results`. The existing five untracked beta test documents are unrelated and must remain untouched. Preserve the newer dependency changes on this base. Do not change operator API/CRDs, native snapshot controllers, image defaults, release workflows or Nodezi in this slice.

The full read-only native provenance proposal is retained by the coordinator at `/tmp/nodezi-delivery-20261029/backups/native-export-attempt-plan.md`. A later native PR must add permanent attempt authority, source holds, mounted-worker authorization and descriptor publication; a finalizer and a digest alone cannot prove the source incarnation.

## Approach and interfaces

- Preserve the existing `Exporter` interface, including `Upload(dir,bucket,name,opts...) error`, and legacy CLI behavior.
- Add context-aware `UploadWithResult(ctx,dir,bucket,name,opts...) (UploadResult,error)` on the concrete S3/GCS exporters, with a small production-consumed result-uploader interface for the CLI. Legacy Upload delegates without changing its established caller contract.
- `UploadResult` is versioned and identifies layout (`single` or `split`), actual object count and compression. A completed single archive additionally contains the exact final object key, lowercase SHA-256 and positive compressed byte count. Split output has no single-object key/digest eligibility, including conservative estimate-triggered split paths that happen to create one object.
- Represent temporary cleanup remaining after a confirmed final upload separately from upload failure, for example with a bounded `cleanupRequired` boolean. Do not overwrite a confirmed final object merely to retry temporary cleanup. No raw provider errors or credential material belong in result JSON.
- Hash and count the serial compressed output after compression and before concurrent chunk dispatch. Include all tar/compressor trailers, propagate close/read failures and wait for EOF. Do not hash retry reads, concurrent callbacks, uncompressed input or provider metadata.
- S3 single-object success requires acknowledged CompleteMultipartUpload. A lost/failed final acknowledgement returns no successful result. Preserve multipart-to-one-object support.
- GCS single-object success requires acknowledged final compose/copy, with original chunk order through every composition round. Propagate intermediate/final composition failures; logging them and continuing is insufficient.
- Cancel and join uploader workers before returning errors. All new work uses the supplied context; no independent retry loop is added.
- Add an optional upload CLI `--result-file` flag, with no implicit output path. When requested, use the result API and write bounded JSON only after successful upload. The encoded result is limited to 3,072 bytes; invalid/oversized output or file-write failure exits unsuccessfully. Existing invocations without the flag retain their behavior. The CLI passes its Cobra context to the new API.
- Define the exact JSON keys and validation in code/tests/docs before consumers depend on them. Native attempt/Job/Pod identity envelopes and worker-gate flags belong to the later provenance PR; this result alone is never accepted as a provenance-backed descriptor.

## Critical files and ownership

- `pkg/dataexporter/exporter.go`: compatible result types and production result-uploader interface.
- `pkg/dataexporter/s3.go`, `gcs.go`: serial byte accounting, context cancellation, actual completion/error handling and worker joins.
- `pkg/dataexporter/compress.go`, `options.go`, `utils.go` only when required to propagate the real byte-stream/compression contract; avoid unrelated refactoring.
- `cmd/dataexporter/cmd/upload.go`: optional result-file path, passed context, bounded successful result output.
- `cmd/dataexporter/cmd/root.go`, `s3.go`, `gcs.go` only for concrete factory/CLI integration needs; preserve legacy standalone provider settings.
- Existing exporter/CLI tests and focused new result tests: use current HTTP/byte fixtures and real completed object bytes.
- Current, unversioned backup/CLI documentation: describe result semantics and their provenance limitation. Do not rewrite historical versioned docs.

No new dependency, provider abstraction solely for tests, controller, database, CRD, release, tag, default image update or infrastructure change is expected.

## Verification

Write meaningful behavior tests first and record genuine failures separately from missing-interface compile failures. Reuse existing S3 byte and GCS HTTP fixtures.

- Independently hash actual completed object bytes for none/gzip/zstd/lz4 and compare exact key, count and digest.
- Cover S3 multipart completion and GCS multi-round composition.
- Cover split output without a restorable single-object claim, including conservative splitting.
- Reader/compressor failure, failed part and intermediate/final composition or completion uncertainty return no successful result.
- Cancellation joins active workers; completed output plus temporary cleanup failure is distinguishable and does not repeat the final upload.
- CLI result flag preserves legacy calls, obeys context cancellation, writes the exact bounded result, and fails on oversized/invalid results or output-write errors. Check actual output/exit behavior, not private helper invocation counts.
- Build/vet/lint changed owners locally; broad suites belong to CI. No live cloud or full CSI provenance claim is made.

Prefer existing HTTP fixtures without containers. If a focused test genuinely needs containers, coordinate with the Nodezi backend owner first, record a fresh inventory and use the audited owned-resource runner. Remove only task-created exact IDs, anonymous volumes and networks on success/failure/interruption. Never run Docker prune or a broadly destructive cleanup target.

## Delivery and limits

Estimated first-slice scope: roughly 8–14 authored files and 800–1,800 changed lines including tests, subject to existing fixture shape. One native implementation owner, then fresh cross-vendor review. Save exact result schema, test evidence and ownership hashes before review.

The coordinator may prepare a separate reviewable PR on `main`; merge, image publication, release and production deployment remain outside authorization. This slice alone does not make a native export restore-eligible. Nodezi A/B continues with restore disabled; the later provenance and Nodezi C contracts remain independent gates.
