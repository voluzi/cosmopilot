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
(`ghcr.io/voluzi/cosmosigner:0.1.0`), configured via the `-cosmosigner-image` / `COSMOSIGNER_IMAGE`
operator flag — see [Configuration](../getting-started/configuration.md#cosmosignerimage). Set
`.spec.cosmosigner.image` to pin or override the image for one specific signer only (e.g. to test a
newer version, such as `latest`, before rolling it out more broadly).
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

  The top-level signer targets at most one validator, so a validator group with more than one
  instance cannot be targeted here — use a per-group signer (below) instead.

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
    backend:
      vault:
        address: https://vault:8200
        keyName: mychain-validator
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
    backend:
      vault:
        address: https://vault:8200
        keyName: my-validator
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
        backend:
          vault:
            address: https://vault:8200
            keyName: chain-validator-a   # distinct key per validator
            tokenSecret: { name: vault-cosmosigner-token, key: token }
    - name: validator-b
      instances: 1
      validator: {}
      cosmosigner:                       # signer "<nodeset>-validator-b-signer"
        replicas: 3
        backend:
          vault:
            address: https://vault:8200
            keyName: chain-validator-b   # must differ from validator-a's key
            tokenSecret: { name: vault-cosmosigner-token, key: token }
```

Each signer holds a distinct consensus identity. Two signers may **not** reference the same Vault
key, GCP key version, or software key secret — the webhook rejects it, since that would let two
validators double-sign.

### Multi-instance validator groups

A multi-instance validator group is N distinct validators, so a per-group `cosmosigner` on it deploys
**one signer per instance** (`<nodeset>-<group>-<index>-signer`). Each instance needs its own
consensus key:

- **Vault**: instance `i` uses the **index-appended** transit key `<keyName>-<i>`. Pre-provision one
  key per instance (e.g. for a 3-instance group with `keyName: myval`, create `myval-0`, `myval-1`,
  `myval-2`). With `uploadGenerated`, Cosmopilot imports instance `i`'s generated key into
  `<keyName>-<i>`.
- **Software**: instance `i` mounts its own generated key secret `<nodeset>-<group>-<i>-priv-key`.
- **GCP KMS** cannot be used on a multi-instance group: a `keyVersion` is a full resource path that
  cannot be derived per instance, so the webhook rejects the combination. Split the group into
  single-instance validator groups, each with its own `keyVersion`.

```yaml {5-11}
spec:
  nodes:
    - name: validators
      instances: 3                       # 3 validators -> 3 signers
      validator: { ... }
      cosmosigner:
        replicas: 3
        backend:
          vault:
            address: https://vault:8200
            keyName: myval               # instance i signs with "myval-<i>"
            tokenSecret: { name: vault-cosmosigner-token, key: token }
```

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
      mount: transit             # optional, defaults to "transit"
      tokenSecret: { name: vault-cosmosigner-token, key: token }
      certificateSecret: { name: vault-ca, key: ca.crt }   # optional CA
      autoRenewToken: true       # optional token-renewer sidecar
      # uploadGenerated: true    # testnets only: import the validator's generated key into Vault.
      #                          # Requires targeting a validator (init/create-validator) so the
      #                          # imported key matches the one registered on-chain. Defaults to false.
```

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

## Migrating from TmKMS

`Cosmosigner`'s Vault backend can point at the **same transit key** a `TmKMS` validator already uses.
To migrate, remove the `.spec.validator.tmKMS` block and add an equivalent `.spec.cosmosigner` block
with `backend.vault.keyName` set to the same key. No key material is moved. `Cosmopilot` removes the
`TmKMS` sidecars and deploys the signer `StatefulSet`; the node keeps listening on the same privval
address.

:::warning[Switching signers]
As with any signer change, ensure the previous signing path is fully stopped before the new signer
connects, to avoid a brief window where two signers could sign. CometBFT's privval handshake allows
only one signer connection per node at a time. A freshly deployed signer starts with empty
double-sign-protection state, so protection ramps up from the first signed block.
:::
