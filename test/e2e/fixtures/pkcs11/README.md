# SoftHSM signer fixture

This opt-in kind test uses the unreleased `edge-pkcs11` binary in a Debian bookworm
image with SoftHSM2, OpenSC and curl. The fixture pod creates a fresh, sensitive,
non-extractable Ed25519 token key. A separate Nibiru pod builds external genesis
with that consensus public key through `gentx --pubkey`; no token private key is
exported or imported by Cosmopilot.

From the repository root:

```sh
docker build -t cosmopilot-pkcs11-test:local test/e2e/fixtures/pkcs11
PKCS11_TEST_IMAGE=cosmopilot-pkcs11-test:local INSTALL_VAULT=false \
  make test.e2e COSMOSIGNER_IMAGE=cosmopilot-pkcs11-test:local \
  CLUSTER_NAME=cosmopilot-pkcs11 REUSE_CLUSTER=false \
  FOCUS='PKCS11 SoftHSM' PROCS=1
```

The suite loads local images into kind using its existing image-loading path.
Its default kind configuration publishes ports 80 and 443. If those are occupied,
create a disposable kind cluster without host port mappings and set
`REUSE_CLUSTER=true`; explicitly delete that cluster when the run finishes.
Use a unique cluster name, record Docker containers, images, volumes and networks
before the run, and remove the task-built images and task-created resources after
both successful and failed runs. Do not target a real cluster.

The assertions cover manually correcting a mismatching `publicKey` before the signer
ever becomes ready: edit the spec, delete its StatefulSet and the wrong-key
ConsensusKeyReservation, and keep its PVCs. Before foreground deletion, set the
StatefulSet's PVC retention policy to `whenDeleted: Retain` and wait for its PVCs'
StatefulSet owner references to disappear; wait for the pods to terminate before
deleting the reservation. They also cover validator block
production, the supplied key in signer status, one signer replica, a persistent binding marker, and a wrong PIN remaining
live but unready with zero Kubernetes restarts for 30 seconds. Correcting the PIN
Secret and explicitly replacing the held pod must resume blocks with the same
Raft PVC and marker. Run the Ginkgo CLI with `--repeat=9` for ten runs; Ginkgo
rejects `go test -count=10`.

For manual inspection, keep the disposable cluster only when deliberately debugging,
inspect the signer pod's `/livez` and `/readyz` endpoints and restart count, update
`token-pin`, delete the held pod, and confirm height advances again. Real HSM
module compatibility, PIN policy, determinism, concurrent sessions and network
failover need a separate hardware drill.
