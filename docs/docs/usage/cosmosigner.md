# Using Cosmosigner

[`Cosmosigner`](https://github.com/voluzi/cosmosigner) is a Go-native CometBFT remote signer. It keeps
your consensus key off the validator node and signs blocks over the network, with:

- **Multiple backends** — HashiCorp Vault Transit, Google Cloud KMS, or a local software key.
- **High availability** — an embedded raft cluster elects a single leader that signs; a lost quorum
  fails closed (downtime) rather than risking a double-sign.
- **Node fan-out** — one signer identity can sign for a whole group of nodes (sentry-style), each of
  which acts as a signing endpoint.

Unlike [`TmKMS`](./tmkms.md), which runs as a sidecar inside the validator pod, `Cosmosigner` runs as a
separate `StatefulSet` that **dials** the targeted nodes' privval address. `Cosmopilot` deploys and
wires everything for you.

:::tip[Image]
The signer image defaults to the operator-wide `cosmosignerImage` Helm value
(`ghcr.io/voluzi/cosmosigner:0.2.1`), configured via the `-cosmosigner-image` / `COSMOSIGNER_IMAGE`
operator flag — see [Configuration](../getting-started/configuration.md#cosmosignerimage). Set
`.spec.cosmosigner.image` to pin or override the image for one specific signer only. Cosmopilot's
managed signing path requires Cosmosigner 0.2.0 or newer for Vault key-version pinning and startup
public-key verification. For production validators, use an immutable image digest rather than a
mutable tag so a rescheduled replica cannot pick up different code without a managed migration.
:::

:::warning[node-utils compatibility]
Cosmosigner discovery gating requires node-utils 2.10.0 or newer. If you override or pin the Helm
`nodeUtilsImage` value, keep it at 2.10.0 or later or targeted node Pods cannot pass their startup
gate.
:::

## How it works

`Cosmopilot` deploys, for each configured signer:

- a `StatefulSet` (`<name>-signer`) with one pod per replica and a per-pod PVC for the raft
  double-sign-protection state and the connection key;
- a headless `Service` (`<name>-signer`) that gives each replica stable DNS for raft peering;
- a headless discovery `Service` (`<name>-signer-privval`) that selects the targeted node pods — the
  signer resolves it to find and dial every target;
- a `ConfigMap` with the rendered `config.yaml`.

Targeted nodes are configured with `priv_validator_laddr` so they listen for the signer, and their
local key is **not** mounted. The discovery service publishes not-ready addresses on purpose: a node
with a remote signer blocks at startup until the signer dials in, so gating discovery on readiness
would deadlock.

## Targeting

A `Cosmosigner` can be attached to a **ChainNodeSet** in two places:

- **Top-level `.spec.cosmosigner`** — one signer holding a single consensus identity.
  `.spec.cosmosigner.nodeGroups` selects which node groups it signs for:
  - A **regular node group** — the group's nodes become the signing endpoints of a single validator
    identity (sentry mode). This lets a group of full nodes validate.
  - The **validator** — leave `nodeGroups` empty to target the `.spec.validator` (a drop-in remote
    signer for a single validator).

  The top-level signer targets at most one validator. A multi-instance validator group is a valid
  target: it counts as ONE validator whose instances are redundant signing endpoints (see
  [High-availability validators](#high-availability-validators-multiple-instances-one-identity)).

- **Per-group `.spec.nodes[].cosmosigner`** — a signer scoped to its enclosing group. Its target is
  fixed to that group (so `nodeGroups` is not allowed). This is how you run **several signed
  validators in one ChainNodeSet** (see [Multiple validators](#multiple-validators-one-signer-each)).

On a standalone **ChainNode**, `.spec.cosmosigner` targets that node; `nodeGroups` is not used.

## Sentry mode: a group of full nodes that validates

```yaml {8-14}
apiVersion: cosmopilot.voluzi.com/v1
kind: ChainNodeSet
metadata:
  name: mychain
spec:
  app: { ... }
  genesis: { ... }
  cosmosigner:
    nodeGroups: [fullnodes]      # the fullnodes group is the signing endpoint
    replicas: 3                  # odd number for raft HA
    raftTLSSecret: cosmosigner-raft-tls
    backend:
      vault:
        address: https://vault:8200
        keyName: mychain-validator
        keyVersion: 1
        tokenSecret: { name: vault-cosmosigner-token, key: token }
  nodes:
    - name: fullnodes
      instances: 3
  # no validator block required
```

The three `fullnodes` all listen for the signer; the raft leader produces exactly one signature per
height and every node relays it. The chain validates using the single consensus identity held in
Vault.

## Drop-in remote signer for a validator

```yaml {6-11}
spec:
  validator:
    info: { moniker: my-validator }
  cosmosigner:                    # nodeGroups empty -> targets the validator
    replicas: 3
    raftTLSSecret: cosmosigner-raft-tls
    backend:
      vault:
        address: https://vault:8200
        keyName: my-validator
        keyVersion: 1
        tokenSecret: { name: vault-cosmosigner-token, key: token }
```

## Multiple validators, one signer each

To run several signed validators in a single ChainNodeSet, give each **validator group** its own
`cosmosigner` block. Cosmopilot deploys one signer per validator:

```yaml {5-12,17-24}
spec:
  nodes:
    - name: validator-a
      instances: 1
      validator: {}
      cosmosigner:                       # signer "<nodeset>-validator-a-signer"
        replicas: 3
        raftTLSSecret: validator-a-cosmosigner-raft-tls
        backend:
          vault:
            address: https://vault:8200
            keyName: chain-validator-a   # distinct key per validator
            keyVersion: 1
            tokenSecret: { name: vault-cosmosigner-token, key: token }
    - name: validator-b
      instances: 1
      validator: {}
      cosmosigner:                       # signer "<nodeset>-validator-b-signer"
        replicas: 3
        raftTLSSecret: validator-b-cosmosigner-raft-tls
        backend:
          vault:
            address: https://vault:8200
            keyName: chain-validator-b   # must differ from validator-a's key
            keyVersion: 1
            tokenSecret: { name: vault-cosmosigner-token, key: token }
```

Each signer holds a distinct consensus identity. Two signers may **not** reference the same Vault
key, GCP key version, or software key secret — the webhook rejects it, since that would let two
validators double-sign.

:::note[One group = one validator identity]
A signer holds a single consensus identity, so a **validator group with a `cosmosigner` is always
ONE validator** — even with `instances > 1` (see below). To run N distinct validators, declare N
validator groups as above, each with its own signer and key.
:::

## High-availability validators: multiple instances, one identity

A validator group with a `cosmosigner` may run **multiple instances**. They are **redundant signing
endpoints of the same validator**, not extra validators: the group's single signer
(`<nodeset>-<group>-signer`) holds the one consensus key and dials **all** instance pods — exactly
like sentry-mode fan-out, but the group *is* the validator. The raft leader produces exactly one
signature per height, so there is no double-signing risk, and the validator keeps signing while
individual nodes restart or catch up.

```yaml {4-5}
spec:
  nodes:
    - name: validator
      instances: 3                       # 3 redundant nodes, ONE validator
      validator: {}
      cosmosigner:
        replicas: 3
        raftTLSSecret: validator-cosmosigner-raft-tls
        backend:
          vault:
            address: https://vault:8200
            keyName: chain-validator     # the group's single consensus identity
            keyVersion: 1
            tokenSecret: { name: vault-cosmosigner-token, key: token }
```

Only instance 0 runs the validator's key flow (genesis init or `createValidator`); the other
instances join as ordinary nodes of the same identity. An explicit `validator.privateKeySecret` on
such a group names that single identity (e.g. as the Vault `uploadGenerated` import source) — the
nodes themselves mount no local key.

Without a `cosmosigner`, a multi-instance validator group keeps its usual meaning: N distinct
validators, one per instance, each with its own generated key.

:::note[One signer per group]
A node group can be signed by only one signer: you cannot list a group in the top-level
`.spec.cosmosigner.nodeGroups` **and** give it its own `.spec.nodes[].cosmosigner`.
:::

## Backends

### Vault Transit

```yaml
cosmosigner:
  backend:
    vault:
      address: https://vault:8200
      keyName: my-validator      # transit key name
      keyVersion: 1              # immutable key version; never follow Vault's latest version
      mount: transit             # optional, defaults to "transit"
      tokenSecret: { name: vault-cosmosigner-token, key: token }
      certificateSecret: { name: vault-ca, key: ca.crt }   # optional CA
      # uploadGenerated: true    # testnets only: import the validator's generated key into Vault.
      #                          # Requires targeting a validator (init/create-validator) so the
      #                          # imported key matches the one registered on-chain. Defaults to
      #                          # false, but is implied when the target initializes a new genesis.
```

Cosmosigner renews renewable and periodic Vault tokens itself at half their current TTL. No
`vault-token-renewer` sidecar is deployed for this backend. Startup rejects a finite non-renewable
token because it cannot remain valid for a long-running validator; use a renewable or periodic token
with permission to renew itself. Non-expiring tokens are accepted; Cosmosigner still polls token
metadata so Secret-backed token replacement is detected, but it sends no renewal request.

Changing a referenced credential or CA Secret **name/key** is a managed lifecycle migration. An
in-place Secret data update does not restart the signer. Cosmosigner reloads a replacement Vault token
after the old token fails lookup, but TLS CA and other client configuration are loaded at process
startup; use a new Secret name when those values rotate so Cosmopilot performs break-before-make.

`keyVersion` is pinned into every public-key lookup and signing request. Rotating the Vault Transit
key therefore does not silently change the validator identity on restart. Deliberately moving to a
new version is a managed signer migration and must match the key the chain expects.

:::note[Genesis init implies `uploadGenerated`]
When the signer targets a validator that initializes a new genesis (`validator.init`),
`uploadGenerated` is treated as `true` even if you leave it unset. A fresh genesis always generates
its consensus key locally, so a pre-provisioned Vault key can never be used there — the generated key
must be imported for the signer to hold it. The `keyVersion: 1` requirement below therefore applies
to every genesis-initializing validator, not only to the explicit opt-in.
:::

`uploadGenerated` creates version 1 of a previously unused Transit `keyName`; set `keyVersion: 1`.
After a completed import, the source Secret is immutable for that target. To import different key
material, choose a new `keyName` and perform a managed migration. Cosmopilot rejects an in-place
source-key change before stopping the serving signer because Vault cannot overwrite an existing
Transit identity.

:::note[Key provenance]
When cosmosigner targets a validator, the signer uses the **validator's own consensus key** — with
the software backend it references the validator's private-key secret, and with Vault
`uploadGenerated` it imports that same key. When no validator is targeted (a sentry-mode signer over
regular groups), you must supply the key yourself: set `backend.software.privateKeySecret`, or
pre-provision the Vault/GCP key. This guarantees the signer signs with exactly the key registered
on-chain.
:::

### Google Cloud KMS

```yaml
cosmosigner:
  serviceAccountName: cosmosigner   # KSA bound to the Google SA (Workload Identity)
  backend:
    gcpKms:
      keyVersion: projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1
      # credentialsSecret omitted -> Workload Identity / ADC
```

:::note[Workload Identity]
When `credentialsSecret` is omitted, the signer authenticates via Application Default Credentials.
On GKE with Workload Identity, set `serviceAccountName` to the Kubernetes service account bound to
the Google service account that has `cloudkms.signerVerifier` on the key — the namespace default
service account is usually not bound.
:::

### Software (testing)

```yaml
cosmosigner:
  backend:
    software:
      privateKeySecret: my-validator-priv-key   # optional when targeting a validator (its own key
                                                 # is used); required for a sentry-mode signer
```

:::warning[Sentry-mode software keys are never minted]
For a sentry-mode signer (no validator targeted) the referenced secret must already exist and hold a
consensus key that is registered on-chain — list it in `validator.init.genesisValidators` so it is
created **before** genesis, or provision it yourself for an externally-registered key. `Cosmopilot`
refuses to mint a fresh key here: the signer only ever deploys after genesis is fixed, so a minted
key could never be in the validator set.
:::

## High availability

Set `replicas` to an odd number (3 tolerates 1 failure, 5 tolerates 2). Each replica runs an embedded
raft node and keeps its own state PVC. Only the raft leader dials the nodes and signs; on leader loss
another replica takes over. There is no HTTP health endpoint, so `Cosmopilot` uses a TCP probe against
the raft port.

Multi-replica signers require `raftTLSSecret`, containing `tls.crt`, `tls.key`, and `ca.crt`, so Raft
membership and state replication use mutual TLS. `unsafeAllowInsecureRaft: true` is an explicit opt-out
for isolated test networks only and cannot be combined with `raftTLSSecret`.

### Raft TLS Secret

Provision the Secret in the same namespace before applying a multi-replica signer. The certificate
must be valid for both client and server authentication and for every per-pod Raft DNS name:
`<signer>-<ordinal>.<signer>.<namespace>.svc`. A wildcard SAN such as
`*.<signer>.<namespace>.svc` covers every ordinal. The signer resource name is `<chainnode>-signer`
for a standalone `ChainNode`, `<chainnodeset>-signer` for a top-level `ChainNodeSet` signer, or
`<chainnodeset>-<group>-signer` for a group signer.

For example, cert-manager can issue one shared certificate from an existing internal CA issuer:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: nibiru-testnet-cosmosigner-raft
  namespace: default
spec:
  secretName: nibiru-testnet-cosmosigner-raft-tls
  dnsNames:
    - nibiru-testnet-signer.default.svc
    - "*.nibiru-testnet-signer.default.svc"
  usages:
    - server auth
    - client auth
  issuerRef:
    name: internal-ca
    kind: ClusterIssuer
```

The resulting Secret must contain `tls.crt`, `tls.key`, and `ca.crt`; use a CA-backed issuer that
populates the CA chain. Adjust the namespace and signer name to match the managed resources.

## Migrating from TmKMS

`Cosmosigner`'s Vault backend can point at the **same transit key** a `TmKMS` validator already uses.
To migrate, remove the `.spec.validator.tmKMS` block and add an equivalent `.spec.cosmosigner` block
with `backend.vault.keyName` set to the same key. No key material is moved. `Cosmopilot` removes the
`TmKMS` sidecars and deploys the signer `StatefulSet`; the node keeps listening on the same privval
address.

## Updating and migrating a signer

Every signer lifecycle change uses a managed break-before-make migration. This includes image,
resources, log level, credentials, backend/key, target groups, software-key Secret, and manifest
placement changes:

1. Cosmopilot preflights the destination key and configuration while the current signer remains up.
2. It scales the signer StatefulSet to zero and waits for the StatefulSet controller to observe zero.
3. It directly lists signer pods and waits until every pod is gone, including terminating pods.
4. It deletes the StatefulSet, confirms it is absent, and lists pods again before recreation.
5. If the destination reports the same public key, the existing raft-state PVCs are retained. A
   different sentry key resets its signer state; a validator-targeted signer is rejected if its key
   differs from the public key already recorded on-chain or by the serving signer.
6. Only then are the new signer configuration and targets applied and the StatefulSet recreated.

Raft-state PVCs carry a Cosmopilot finalizer so a normal deletion cannot silently replace slash state
while the StatefulSet can still create pods. If an established signer's required claim is missing,
terminating, unbound, foreign, or unprotected, Cosmopilot latches that StatefulSet at zero replicas.
Automated recovery requires completing a persisted different-key reset. Restoring the original
volume instead requires verifying it out of band and explicitly removing the
`cosmopilot.voluzi.com/cosmosigner-retained-state-lost` StatefulSet annotation.
During upgrades, bound non-terminating claims already labelled for the same owner are protected before
any other signer preflight runs. Claims are released only after signer pods and the StatefulSet are
gone; deleting the owning `ChainNode` or `ChainNodeSet` waits for that ordered cleanup, and any unrelated
PVC finalizer can intentionally delay completion. The StatefulSet also records monotonic rollout
evidence so restoring incomplete CR status cannot make a previously serving signer look like a fresh
deployment.

Replica-count and state-storage changes remain unsupported because they require an explicit raft
membership or PVC migration.

### What a migration looks like while it runs

A migration in a `ChainNodeSet` retargets its nodes with the signer stopped, so for a few minutes the
signer pod is gone and `kubectl get endpoints <signer>-privval` returns `not found`. **This is the
expected shape of a healthy migration, not a failure.** The discovery Service is deleted on purpose,
so stale endpoints cannot reconnect the recreated signer to its previous targets.

How long this takes depends on what changed. Most migrations — image, resources, log level,
credentials — keep the same targets, so the discovery labels already match and this phase passes
straight through. When only the target label changes, Cosmopilot patches it onto the running pod
in place, with no restart.

Target pods are **recreated** only when the pod spec changes too — most visibly when a node switches
between local and remote signing, which adds or removes `priv_validator_laddr` and the local key
mount. In that case the label is applied together with the new spec rather than patched onto the
running pod, so a pod still on the previous signing path is never exposed to the new signer while the
old one is live. That is the case where this phase takes minutes rather than seconds.

Cosmopilot names the step it is waiting on in both its logs and `CosmosignerRetargeting` events on the
`ChainNodeSet`:

```shell
kubectl describe chainnodeset <name> | grep CosmosignerRetargeting
```

```
Normal  CosmosignerRetargeting  waiting for 2 target pod(s) to pick up their new signer discovery
                                label: cp-nodes-validators-0, cp-nodes-validators-1
```

During the same window the targeted nodes themselves restart while they wait for the signer to dial
in. A node that logs `can't get pubkey: endpoint connection timed out` and restarts before the signer
is up is expected: the node and signer rendezvous by retrying, and the pair converges once both are
running.

### What a first rollout looks like

A fresh deploy shows a similar pattern, for a different reason. Nodes and their signer start at the
same time and find each other by retrying, so before they converge you will typically see:

- the node restart once or twice with `can't get pubkey: endpoint connection timed out` — it blocks on
  its remote signer at startup and exits if the signer has not dialed in yet;
- the signer log `resolve target nodes … no such host` until the targeted pods have DNS records.

Both are retried and clear on their own. Cosmosigner re-resolves its targets as soon as a connection
drops, rather than only on its reconcile interval, so a node replaced with a new pod IP is picked up in
about a second and the pair normally converges within a few seconds.

That behavior needs **Cosmosigner 0.2.1 or newer**, which is the default `cosmosignerImage`. If you
have pinned `.spec.cosmosigner.image` to 0.2.0 or earlier, re-resolution happens only on the fixed
interval, so a node that churns during rendezvous can restart several times and take a few minutes to
settle. The rollout is healthy either way; only how long it looks unsettled differs.

**A genesis-initializing validator group additionally shows one pod recreation:** create → `Error` →
recreate. This is deliberate and is the safety mechanism working, not a defect. Such a validator
generates its consensus key itself during bootstrap, so the key does not exist when the pod is first
created and the signer cannot yet hold it. The pod therefore starts on its **local** signing path, and
only once the key exists does Cosmopilot switch it to the signer — a change of pod spec, so the pod is
recreated rather than patched.

That recreation is what makes the switch safe: the local-key pod is deleted and gone before the
signer-targeted pod is created, so the consensus key is never live in two places at once. Marking the
pod as signer-targeted any earlier would label a pod that still holds a local key. By contrast,
sentry groups and validators against an existing genesis are targeted from creation and show no such
recreation.

## Consensus-key reservations

Before importing a key, retargeting nodes, or creating signer pods, Cosmopilot atomically creates a
cluster-scoped `ConsensusKeyReservation` keyed by chain ID and canonical public key. A different
`ChainNode` or `ChainNodeSet` root cannot claim that same chain/key pair, even if it would use separate
Raft state. Independent claims inside one `ChainNodeSet` are also rejected, while a local, TmKMS, and
Cosmosigner migration for the same logical validator shares one claim. This closes the cross-resource
and same-root double-sign windows during migrations and upgrades.

Helm installs files from a chart's `crds/` directory on first install, but does not upgrade or add them
on `helm upgrade`. Existing installations must apply the CRDs from the target chart before upgrading
the controller:

```shell
helm show crds oci://ghcr.io/voluzi/helm/cosmopilot --version <target-version> | kubectl apply -f -
```

Confirm `consensuskeyreservations.cosmopilot.voluzi.com` exists before starting the new controller.
Without it, reservation-aware reconciliation fails closed: new signing paths are not created, but
already-running validators may remain online until the CRD is installed.

Do not change a validator signing configuration while old and new Cosmopilot controller versions are
running together during a rolling operator upgrade. Reservations are atomic among reservation-aware
controllers, but an older controller does not consult them. Apply the CRD, finish the controller
rollout, and only then begin a local/TmKMS/Cosmosigner migration.

Reservations are released automatically by a dedicated finalizer on the owning `ChainNode` or
`ChainNodeSet`. Cosmopilot first prevents the retired claim from being recreated, requests deletion
of its managed local-validator, TmKMS, and Cosmosigner workloads, and waits until the relevant
`ChainNode`, Pod, Job, and StatefulSet objects are absent. It then deletes only reservations whose
immutable owner UID and claim match, using the reservation object's UID as a deletion precondition.
Retained key Secrets, node-data PVCs, and Cosmosigner raft PVCs are inert state and do not by
themselves block reservation release.

A resource recreated with the same namespace/name but a new UID stays blocked until the old signing
path is absent. Once verified, the stale reservation is recovered and the replacement acquires a new
reservation normally. Ambiguous or foreign workloads fail closed and produce a
`ConsensusKeyReservationBlocked` event.

Use `kubectl get ckr` to inspect reservations. Manual deletion is reserved for exceptional recovery
where the controller cannot safely attribute old resources. Before deleting one manually, prove that
every former signing process is stopped and cannot restart, inspect the immutable owner UID and claim,
and delete only the exact object:

```shell
kubectl get ckr <reservation-name> -o yaml
kubectl delete ckr <reservation-name>
```

Deleting a reservation manually permits another controller root to claim the key; doing so while an
old path can still sign can create independent double-sign state for the same validator.

:::warning[Cosmos does not rotate the validator key]
Cosmopilot does not submit an on-chain consensus-key rotation. Once validator status or a serving
signer records the validator public key, a managed Cosmosigner migration must resolve to that same
key. Perform consensus-key rotation through the chain's supported governance/validator procedure,
not by changing the managed signer backend.
:::

:::warning[Slash-protection state at implementation boundaries]
Break-before-make prevents two signing implementations from running concurrently, and Cosmosigner
retains its Raft high-water mark across same-key Cosmosigner upgrades. It cannot import historical
`priv_validator_state.json` from a local validator or TmKMS. Before the first migration into
Cosmosigner, stop the old path cleanly, ensure the validator data cannot roll back below the last
signed height, and retain the old signing state for incident recovery. A public-key match alone does
not transfer slash-protection history, so Cosmopilot refuses to remove a validator-serving
Cosmosigner back to an independent local or TmKMS engine. That handoff requires a future explicit
quiesce, slash-state transfer, and verification protocol.
:::
