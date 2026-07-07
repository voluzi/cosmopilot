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
`Cosmosigner` is currently published at `ghcr.io/voluzi/cosmosigner:latest`. Pin a released tag with
`.spec.cosmosigner.image` once versioned releases are available.
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

On a **ChainNodeSet**, `.spec.cosmosigner.nodeGroups` selects which node groups the signer signs for:

- A **regular node group** — the group's nodes become the signing endpoints of a single validator
  identity (sentry mode). This lets a group of full nodes validate.
- The **validator** — leave `nodeGroups` empty to target the `.spec.validator` (a drop-in remote
  signer for a single validator).

A validator group with more than one instance cannot be targeted: each instance is a distinct
validator with its own key, which cannot be collapsed onto one signer identity.

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
  backend:
    gcpKms:
      keyVersion: projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1
      # credentialsSecret omitted -> Workload Identity / ADC
```

### Software (testing)

```yaml
cosmosigner:
  backend:
    software:
      privateKeySecret: my-validator-priv-key   # optional; generated if absent
```

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
