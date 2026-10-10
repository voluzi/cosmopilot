# Using Cosmosigner

[`Cosmosigner`](https://github.com/voluzi/cosmosigner) is a Go-native CometBFT remote signer. It keeps
your consensus key off the validator node and signs blocks over the network, with:

- **Multiple backends** — HashiCorp Vault Transit, Google Cloud KMS, AWS KMS, PKCS#11 tokens, or a local software key.
- **High availability** — an embedded raft cluster elects a single leader that signs; a lost quorum
  fails closed (downtime) rather than risking a double-sign.
- **Node fan-out** — one signer identity can sign for a whole group of nodes (sentry-style), each of
  which acts as a signing endpoint.

`Cosmosigner` runs as a separate `StatefulSet` that **dials** the targeted nodes' privval address
on TCP 26659. `Cosmopilot` deploys and wires everything for you.

:::tip[Image]
The signer image resolves in this order: `.spec.cosmosigner.image`, a nonempty operator-wide
`cosmosignerImage` Helm value (the `-cosmosigner-image` / `COSMOSIGNER_IMAGE` operator setting),
then the pinned default supplied by the selected manager release. See
[Configuration](../getting-started/configuration.md#cosmosignerimage). Cosmopilot's
managed signing path requires Cosmosigner 3.1.0 or newer for HTTP health endpoints and bounded
redial. An overridden image is not version-checked: with a build older than 3.1.0, `/livez` never
answers, the signer never becomes live, and the validator does not sign until the image is
corrected. For production validators, use
an immutable image digest rather than a mutable tag so a rescheduled replica cannot pick up different code without a managed migration.

`awsKms` requires Cosmosigner 3.2.0 or later, which is the default image.
:::

:::warning[node-utils compatibility]
Use node-utils 4.0.0 or newer with this Cosmopilot release. It includes the `wait-for-signer` startup
gate and the polling-based upgrade coordination used by node Pods without a trace FIFO.
:::

## Applying the local testnet examples

The [sentry example](../examples/nibiru/testnet-cosmosigner-sentry.md) and
[multiple-validator example](../examples/nibiru/testnet-multi-validator-cosmosigner.md) each
include a complete local-chain genesis ConfigMap and publicly known software signing keys.
Apply either file in its own namespace; neither requires Vault or pre-provisioned Secrets.
The sentry example runs one validator identity through three node endpoints. The multiple-validator
example registers three distinct identities, each with its own signer; validator-c has three
redundant node endpoints.

These disposable testnets explicitly use insecure Raft transport. Use fresh private keys and
`raftTLSSecret` for production. All examples need persistent-volume provisioning; no particular
StorageClass is selected.

## How it works

`Cosmopilot` deploys, for each configured signer:

- a `StatefulSet` (`<name>-signer`) with one pod per replica and a per-pod PVC for the raft
  double-sign-protection state and the connection key;
- a headless `Service` (`<name>-signer`) that gives each replica stable DNS for raft peering;
- a headless discovery `Service` (`<name>-signer-privval`) that selects the targeted node pods — the
  signer resolves it to find and dial every target;
- a `ConfigMap` with the rendered `config.yaml`;
- NetworkPolicies for signer Raft traffic and target-node privval traffic.

Targeted nodes listen directly with `priv_validator_laddr = "tcp://0.0.0.0:26659"`, and their local
key is **not** mounted. The discovery Service publishes not-ready addresses so the signer can reach
nodes while their startup gate is waiting.

The final `wait-cosmosigner-discovery` init container listens on 26659, reads at least one byte from
a signer connection, closes it and exits. The app then binds the same port and the signer redials.
A bare TCP connect does not release the gate. The gate times out after 25 seconds with DNS
observations in its error; the controller recreates a failed node Pod.

### NetworkPolicy requirement

The target policy allows TCP 26659 only from this signer's pods in the same namespace. It explicitly
allows all other TCP ports and all UDP/SCTP ports from anywhere, preserving P2P, RPC, and other
node ingress. The CNI must enforce NetworkPolicy with `endPort` support: otherwise TCP 26659 is
open cluster-wide. Privval's SecretConnection does not authenticate the signer, so this policy is
the access control. Other policies selecting the node are additive; ensure they do not also allow
unrestricted ingress to 26659.

### HTTP health probes

Cosmopilot sets `http_addr` and `COSMOSIGNER_HTTP_ADDR` to `0.0.0.0:8080`. Startup and liveness probes
use `/livez`, which answers while HTTP is serving without touching the backend or Raft. Readiness
uses `/readyz`, which passes once the Raft store, binding, and backend preflight are initialized and
fails during shutdown. Followers pass readiness too; readiness does not assert leadership or quorum.
There is no Service for the HTTP port; kubelet probes connect to the Pod directly.

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

Cosmosigner 3.x also records which signer cluster owns the key (see
[Key ownership](#key-ownership-cosmosigner-3x)). Create the registry once, as a KV v2 mount with
automatic version expiry disabled, and give the signer's token the matching policy:

```sh
vault secrets enable -path=cosmosigner -version=2 kv
vault write cosmosigner/config delete_version_after=0s
```

```hcl
path "transit/keys/my-validator"             { capabilities = ["read"] }
path "transit/sign/my-validator"             { capabilities = ["update"] }
path "auth/token/lookup-self"                 { capabilities = ["read"] }
path "auth/token/renew-self"                  { capabilities = ["update"] }
path "sys/capabilities-self"                  { capabilities = ["update"] } # optional
path "cosmosigner/data/cluster-bindings/*"     { capabilities = ["create", "update", "read"] }
path "cosmosigner/metadata/cluster-bindings/*" { capabilities = ["read"] }
```

These three self-service paths are part of Vault's built-in `default` policy, so list them explicitly
only when the token is created with `-no-default-policy`. `lookup-self` is
required: without it the signer refuses to start because it cannot keep the token alive.
`capabilities-self` is optional; when denied, Cosmosigner falls back to a sign probe.

`create` and `update` on `cluster-bindings` are needed only to write the record the first time a
signer cluster starts. To keep them off the running signer, grant the runtime token only `read` there
and set `claimTokenSecret` to a separate token that has them; it is mounted into the signer but used
for that single write. Set `bindingMount` when the registry lives at a path other than `cosmosigner`.
Neither permission reaches Transit key administration: the signer cannot delete or export the key.

```yaml
      bindingMount: cosmosigner                                    # optional
      claimTokenSecret: { name: vault-cosmosigner-claim, key: token }  # optional
```

Cosmosigner renews renewable and periodic Vault tokens itself at half their current TTL. Startup
rejects a finite non-renewable token because it cannot remain valid for a long-running validator; use a renewable or periodic token
with permission to renew itself. Non-expiring tokens are accepted; Cosmosigner still polls token
metadata so Secret-backed token replacement is detected, but it sends no renewal request.

Changing a referenced credential or CA Secret **name/key** is a managed lifecycle migration. An
in-place Secret data update does not restart the signer. Cosmosigner reloads a replacement Vault token
after the old token fails lookup, but TLS CA and other client configuration are loaded at process
startup; use a new Secret name when those values rotate so Cosmopilot performs break-before-make.

`keyVersion` is pinned into every public-key lookup and signing request. Rotating the Vault Transit
key therefore does not silently change the validator identity on restart. Moving an existing signer
to another version of the same `keyName` is rejected, because Cosmosigner 3.x binds the whole key to
the signer cluster that first used it (see [Key ownership](#key-ownership-cosmosigner-3x)): new key
material needs a new `keyName`, and must match the key the chain expects.

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
`uploadGenerated` or GCP KMS `import` it imports that same key. When no validator is targeted (a
sentry-mode signer over regular groups), you must supply the key yourself: set
`backend.software.privateKeySecret`, or pre-provision the Vault/GCP/AWS key. This guarantees the signer
signs with exactly the key registered on-chain.
:::

### Google Cloud KMS

Use `keyVersion` when the consensus key already exists in Cloud KMS:

```yaml
cosmosigner:
  serviceAccountName: cosmosigner   # KSA bound to the Google SA (Workload Identity)
  backend:
    gcpKms:
      keyVersion: projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1
      # credentialsSecret omitted -> Workload Identity / ADC
```

To migrate the targeted validator's existing `priv_validator_key.json` into Cloud KMS without
changing its consensus identity, configure a managed BYOK import instead of `keyVersion`:

```yaml
cosmosigner:
  serviceAccountName: cosmosigner   # KSA bound to a Google SA, or set credentialsSecret below
  backend:
    gcpKms:
      import:
        project: my-project
        location: global             # optional; defaults to global
        keyRing: validators
        key: consensus-key
        importJob: consensus-import  # optional; defaults to <key>-import
        protectionLevel: hsm         # optional; software or hsm; defaults to software
      # credentialsSecret:
      #   name: gcp-kms-credentials
      #   key: credentials.json
```

Managed import is explicit and validator-only. The signer must target a validator that initializes
genesis, uses `createValidator`, or names an existing `privateKeySecret`; sentry-only signers cannot
request an import. Cosmopilot mounts only `priv_validator_key.json` into the one-shot import Pod and
uses the signer's ServiceAccount, image pull secrets, restricted security context, and either
Workload Identity/ADC or `credentialsSecret`.

The import Pod authenticates like the signer: as the Google service account in `credentialsSecret`
when it is set, otherwise as the Google service account bound to the signer's Kubernetes service
account (Workload Identity/ADC). That identity also needs these permissions while importing:

| When | Scope | Permissions |
| --- | --- | --- |
| Import into an existing CryptoKey | CryptoKey | `cloudkms.cryptoKeys.get`, `cloudkms.cryptoKeyVersions.create`, `cloudkms.cryptoKeyVersions.get`, `cloudkms.cryptoKeyVersions.viewPublicKey` |
| Always | Key ring, or the ImportJob if it already exists | `cloudkms.importJobs.get`, `cloudkms.importJobs.useToImport` |
| Named ImportJob does not exist | Key ring | `cloudkms.importJobs.create` |
| CryptoKey must be created | Key ring | `cloudkms.keyRings.get`, `cloudkms.cryptoKeys.create`, plus the CryptoKey permissions above |
| Key ring must be created | Project | `cloudkms.keyRings.create`, plus every permission above at project scope, since the key ring and key do not exist yet to hold the grants (locations cannot hold IAM grants). Simpler: pre-create the key ring and grant the narrower scopes |

Cosmosigner reads the key first and creates only what is missing. An existing key must use
`ASYMMETRIC_SIGN` with `EC_SIGN_ED25519` at the requested protection level. The default
`<key>-import` ImportJob expires after three days, so a new job name is usually needed. Use a custom
role for these import-only permissions, then remove it after the signer has rolled out with the
recorded version.

The import is break-before-make. Cosmopilot quiesces an existing signer, runs the import once, records
the exact `cryptoKeyVersion` returned by Cosmosigner, and then reads that same version back until its
public key is available and matches the source key. Cloud KMS may leave a version in `PENDING_IMPORT`
after accepting it; a successful import Pod alone is therefore **not** completion. Retain the source
Secret and all out-of-cluster backups until the import is verified and the signer has rolled out with
the recorded version. A mismatched key is a hard error and never retargets the validator.

Keep the `gcpKms.import` configuration after verification. Replacing it with `gcpKms.keyVersion`
for the same CryptoKey is not supported, even for the version recorded in status; this changes the
managed import identity and key-discovery path rather than acting as a configuration no-op.

If an ImportJob expires before the import completes, choose a new `importJob` name. Changing the job
does not change the destination consensus identity; changing `project`, `location`, `keyRing`, or
`key` selects a different destination and is handled as a managed signer migration.

:::note[Workload Identity]
When `credentialsSecret` is omitted, the signer authenticates via Application Default Credentials.
On GKE with Workload Identity, set `serviceAccountName` to the Kubernetes service account bound to
the Google service account that has `cloudkms.signerVerifier` on the key — the namespace default
service account is usually not bound.
:::

With Cosmosigner 3.x the signer's Google service account also needs, on the CryptoKey,
`cloudkms.cryptoKeys.get` (to read which signer cluster owns the key) and `cloudkms.cryptoKeys.update`
(to label an unowned key the first time a signer cluster starts; see
[Key ownership](#key-ownership-cosmosigner-3x)). Grant them through a custom role rather than
`roles/cloudkms.admin`. `cryptoKeys.update` changes CryptoKey metadata such as labels; it cannot
destroy or disable key versions. To keep it off the running signer, grant the runtime identity only
`cryptoKeys.get` and set `claimCredentialsSecret` to a service account key that has `update`; it is
used only for that label write.

```yaml
      claimCredentialsSecret: { name: gcp-kms-claim, key: credentials.json }   # optional
```

### AWS KMS

Use `keyId` when the consensus key already exists in AWS KMS. It must be the full immutable key
ARN, rather than an alias, alias ARN or bare key ID:

```yaml
cosmosigner:
  replicas: 3
  raftTLSSecret: cosmosigner-raft-tls
  serviceAccountName: cosmosigner        # Kubernetes service account configured for IRSA
  backend:
    awsKms:
      keyId: arn:aws:kms:eu-west-1:123456789012:key/12345678-1234-1234-1234-123456789012
      region: eu-west-1
      # credentialsSecret omitted -> standard AWS SDK credential chain
      # claimRoleArn: arn:aws:iam::123456789012:role/validator-key-claimer
      # timeout: 10s
```

The same backend block works on a standalone ChainNode, a top-level ChainNodeSet signer, or a
per-group signer. The customer-managed key must be single-region `ECC_NIST_EDWARDS25519` with
`SIGN_VERIFY` usage and `ED25519_SHA_512` support. Use the same AWS account and region for the
signer and key; Cosmosigner rejects multi-region keys. It signs RAW messages, so the full sign
bytes must fit within 4096 bytes, including canonical vote-extension encoding. Chains requiring
larger extensions cannot use this backend.

Without `credentialsSecret`, the standard AWS SDK credential chain applies. For IRSA, configure
`serviceAccountName` with the AWS role binding and web-identity setup; the same service account is
used by the signer and its one-shot public-key discovery Pod. A Google Workload Identity binding
alone does not grant AWS access.

For static credentials, create a Secret key containing an AWS shared-credentials INI file with a
`[default]` profile, then reference it:

```yaml
      credentialsSecret: { name: aws-kms-credentials, key: credentials }
```

Only that Secret key is projected into a read-only directory mount, as `/aws/credentials`.
Cosmopilot sets `AWS_SHARED_CREDENTIALS_FILE=/aws/credentials` on the signer and public-key Pod;
Cosmosigner uses those credentials for AWS requests, unless the Pod also receives IRSA web-identity
variables through its ServiceAccount: the AWS SDK prefers web identity, so use one or the other. In-place Secret updates reach the mounted
file but do not restart the signer, and SDK file reloading is not guaranteed. Change the Secret
name/key to trigger the existing managed migration when rotating static credentials.

| Operation | AWS permissions on the key |
| --- | --- |
| Runtime signer | `kms:GetPublicKey`, `kms:Sign`, `kms:ListResourceTags` |
| Public-key discovery | `kms:GetPublicKey` |
| First startup claim | `kms:GetPublicKey`, `kms:ListResourceTags`, `kms:TagResource` |

The key policy must also permit the relevant operations. Without `claimRoleArn`, the runtime
identity needs `kms:TagResource` for the first startup claim. With it, the runtime identity needs
`sts:AssumeRole` on that role and the role's trust policy must allow the runtime principal. The
claim role needs the first-startup-claim permissions above. It is used only by signer startup and
is not passed to the public-key discovery Pod. These are AWS permissions, not Kubernetes RBAC.

`timeout` is passed through `COSMOSIGNER_AWS_TIMEOUT` to both Pods; when omitted, Cosmosigner uses
its default `10s` AWS request timeout. It is not a YAML backend config field in Cosmosigner.

Cosmopilot discovers the public key with `cosmosigner pubkey` against the configured ARN, following
the same one-shot Pod lifecycle as pre-provisioned `gcpKms.keyVersion`. Cosmosigner verifies that
AWS returns that exact ARN. Cosmopilot reserves the consensus public key and pins it into signer
startup with `--expected-public-key`, so a signer cannot serve a different validator key. The
recorded serving identity is `awskms` plus the key ARN; credentials, region, claim role and timeout
are runtime settings whose changes use the existing break-before-make migration. A migration
preserves Raft state when the public key is unchanged. Changing the ARN does not rotate an
established validator's on-chain key: the discovered public key must still match that validator.

To move an existing validator key into AWS KMS, run `cosmosigner import` yourself under an
administrative identity, following the [Cosmosigner AWS backend instructions](https://github.com/voluzi/cosmosigner#aws-kms-backend).
Then verify it with `cosmosigner pubkey` and configure `keyId` with the resulting key ARN.
Cosmopilot does not provision or import AWS keys. Import acceptance may precede public-key
availability: confirm that the public key matches the original validator key before using it.
Retain a protected recovery backup of the original imported material outside AWS; AWS imported
material cannot be exported, and the customer is responsible for its durability.

A pre-provisioned AWS key cannot be used with `validator.init` or `createValidator`: those flows
register a locally generated consensus key. Use an externally registered validator identity or
sentry mode with a key already registered on-chain.

Cosmopilot enables startup claiming, as it does for the other backends. Three replicas sharing one
Raft history acquire the same cluster ID and write the same `cosmosigner-cluster-id` key tag.
AWS tag writes have no compare-and-set and reads are eventually consistent. Guarantee exclusive
key ownership and externally serialize first adoption across Kubernetes clusters and unmanaged
signers; Cosmopilot's reservation only coordinates its own Kubernetes control domain. Allow prior
claims to become visible. If claim propagation prevents readiness, retry with the same preserved
Raft history. Never remove or rewrite a claim to bypass lost signing history.

### PKCS#11

`backend.pkcs11` selects a pre-existing, sensitive, non-extractable Ed25519 key on a token;
Cosmopilot does not provision or import token keys. Build a signer image from
`ghcr.io/voluzi/cosmosigner:3.2.0-pkcs11` with the vendor's glibc shared library and its dependencies,
and set `image` explicitly. The default image is a static build and does not include the backend.
`cosmosigner version` reports `pkcs11: true` for a supported build; a static build reports its own
unsupported-build error. Cosmopilot does not derive image names; `edge-pkcs11` and `latest-pkcs11`
use `Always` pull policy, while pinned tags and digests retain the ordinary policy.

Obtain the token key's base64 Ed25519 consensus public key with `cosmosigner pubkey --backend pkcs11`
outside the reconcile loop, supplying `--pkcs11-module`, exactly one of `--pkcs11-token-label` or
`--pkcs11-slot`, `--pkcs11-key-label` and/or `--pkcs11-key-id`, `--pkcs11-pin-file` and
`--pkcs11-binding-file` (a marker path in an existing directory); this logs in to the token and makes
one PIN attempt. Supply the resulting key as `publicKey`. Cosmopilot uses that value for validator
identity, consensus-key reservations, on-chain comparison and status, and pins it as the signer's expected
public key. It never creates a PKCS#11 public-key discovery pod, avoiding unattended PIN attempts.
Register that key on-chain or supply an external genesis containing it before using this backend;
local genesis initialization and create-validator flows require software or a managed import.

```yaml
spec:
  cosmosigner:
    image: registry.example.com/cosmosigner-vendor:tested-pkcs11
    replicas: 1
    backend:
      pkcs11:
        module: /opt/vendor/libpkcs11.so
        tokenLabel: validator-token
        # slot: 0                  # exactly one of tokenLabel or slot
        keyLabel: consensus
        keyId: "01a2"              # label and ID intersect when both are set
        pinSecret:
          name: validator-hsm-pin
          key: pin
        publicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" # replace with the token key
    env:
      - name: VENDOR_CLIENT_CONFIG
        value: /vendor/config/client.conf
    volumes:
      - name: vendor-config
        configMap:
          name: validator-hsm-config
    volumeMounts:
      - name: vendor-config
        mountPath: /vendor/config
        readOnly: true
```

Use the vendor's actual environment variables and configuration files; `env`, `volumes` and
`volumeMounts` reach only the signer container, and changing any of them uses the existing full-stop
migration. User `env` entries follow `POD_NAME`, so values can reference `$(POD_NAME)`, and precede
the variables the operator sets, which take precedence. Do not set `POD_NAME` or any `COSMOSIGNER_*`
variable in `env`: they override the signer's identity and its rendered configuration. Do not shadow
operator-managed mounts. A mismatching
`publicKey` leaves the signer in CrashLoopBackOff; a wrong PIN leaves it running but not ready until someone restarts it after
fixing the Secret. Each concurrently starting replica can make one failed PIN attempt. The PIN is a
directory-mounted Secret file, so updates propagate, but the PIN hold requires an explicit restart.

For a wrong module, token selector or key selector, correct the spec, then delete the signer
StatefulSet. For a wrong `publicKey`, correct the spec, then delete the signer StatefulSet and the
ConsensusKeyReservation of the wrong key. If both are wrong, correct them in two separate edits:
admission accepts an initial `publicKey` correction only when nothing else in the backend changes.
Correct the spec first so reconciliation does not restore
the default PVC retention policy. In both cases, before deleting the StatefulSet, set its
`spec.persistentVolumeClaimRetentionPolicy.whenDeleted` to `Retain` and wait for its PVCs' StatefulSet
owner references to disappear; use foreground deletion and wait for its pods to terminate before
deleting the reservation. Keep the signer's PVCs; the ConfigMap does not need deletion. These procedures
are only for a signer that never became ready; validator key changes after serving are refused, and
sentry key changes follow the existing migration rules.

Every replica must reach the same token and key, which in practice requires a network HSM for more
than one replica. The container runs as UID/GID 1000; give it access to the module, client files and
token. Each replica keeps its binding marker at `/data/cluster-binding.json` beside its Raft history
on its own PVC, and same-key migrations preserve both. This marker protects that history, not token
ownership across independent deployments: the existing consensus-key reservation covers managed
owners in this Kubernetes cluster. SoftHSM tests do not establish compatibility, determinism,
concurrent sessions, failover or PIN policy on a real HSM.

### Software (testing)

```yaml
cosmosigner:
  backend:
    software:
      privateKeySecret: my-validator-priv-key   # optional when targeting a validator (its own key
                                                 # is used); required for a sentry-mode signer
```

The key Secret is mounted read-only, so the software backend keeps its ownership marker (see
[Key ownership](#key-ownership-cosmosigner-3x)) on each replica's state PVC, next to the Raft history
it names.

:::warning[Sentry-mode software keys are never minted]
For a sentry-mode signer (no validator targeted) the referenced secret must already exist and hold a
consensus key that is registered on-chain — list it in `validator.init.genesisValidators` so it is
created **before** genesis, or provision it yourself for an externally-registered key. `Cosmopilot`
refuses to mint a fresh key here: the signer only ever deploys after genesis is fixed, so a minted
key could never be in the validator set.
:::

### Key ownership (Cosmosigner 3.x)

Cosmosigner 3.x binds each key to one signer cluster: the first time a signer cluster starts it
records its Raft cluster ID with the key (a Vault KV record, a label on the Cloud KMS CryptoKey, an
AWS KMS key tag, or a marker file for the software backend), and afterwards it refuses to sign
with a key recorded for a different cluster. A visible claim prevents a second independent history from adopting the key;
initial KMS adoption still requires exclusive ownership as described above.

`Cosmopilot` lets the signer write that record itself (`COSMOSIGNER_CLAIM_IF_UNCLAIMED`), because it
already guarantees the precondition: a consensus key is reserved for one signer, and every migration
stops the old signer before the new one starts. A key already recorded for another cluster is never
reassigned; the signer fails to start instead, and the migration stays in `RollingOut` with that error
in the signer logs.

A signer whose Raft state is discarded (a different-key migration, or deleting and recreating the
signer) starts a **new** cluster. If it points at a Vault key name or Cloud KMS CryptoKey that an
earlier cluster already recorded, it is refused: use a new key name or CryptoKey for new key material,
and never reuse one after its Raft state has been removed. Admission rejects the common case, moving
a signer to another `keyVersion` of the same Vault key or CryptoKey. For the same reason every signer
needs its own Vault key or CryptoKey, including signers on different chains: only the first signer
cluster to start can use it.

Upgrading an existing signer from Cosmosigner 0.2.x to 3.x keeps its Raft state, so it records its
existing cluster and keeps signing. Before upgrading the operator, create the Vault KV mount or grant
the Cloud KMS permissions above: the new default image replaces running signers through the usual
break-before-make migration. A signer whose permissions are missing keeps restarting and recovers by
itself once they are granted, but its validator does not sign meanwhile. To upgrade signer by
signer, pin `.spec.cosmosigner.image` to your current 0.2.x image before upgrading the operator, then
remove the pin one signer at a time. Going back to 0.2.x after a signer has run 3.x is unsupported,
because 0.2.x cannot read the state 3.x writes.

## High availability

Set `replicas` to an odd number (3 tolerates 1 failure, 5 tolerates 2). Each replica runs an embedded
raft node and keeps its own state PVC. Only the raft leader dials the nodes and signs; on leader loss
another replica takes over. `Cosmopilot` uses HTTP startup and liveness probes on `/livez` and
a readiness probe on `/readyz`. Followers pass readiness too; it does not assert leadership or quorum.
See [HTTP health probes](#http-health-probes).

Multi-replica signers require `raftTLSSecret`, containing `tls.crt`, `tls.key`, and `ca.crt`, so Raft
membership and state replication use mutual TLS. `unsafeAllowInsecureRaft: true` is an explicit opt-out
for isolated test networks only and cannot be combined with `raftTLSSecret`.

### Running signers highly available

Cosmopilot manages one PodDisruptionBudget per signer, named after its StatefulSet, with
`maxUnavailable: 1` and `unhealthyPodEvictionPolicy: AlwaysAllow` (Kubernetes 1.27+).
A single-replica signer can therefore be evicted during a drain. Adding a second budget selecting
those pods makes Kubernetes refuse their evictions, blocking drains of their nodes until the second
budget is removed. A budget with the signer's name that is not owned by Cosmopilot blocks the signer's
reconcile like any other name collision. Missing policy API access or permissions is a reconciliation
error.
PDBs only gate voluntary evictions; they do not block managed signer migrations.

Set `nodeSelector` and `affinity` on the signer itself; these fields are not inherited from node
specs and do not apply to one-shot import or public-key pods. For example, prefer spreading this
three-replica signer across nodes (replace `mychain-signer` with its actual resource name):

```yaml
cosmosigner:
  replicas: 3
  raftTLSSecret: cosmosigner-raft-tls
  affinity:
    podAntiAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          podAffinityTerm:
            labelSelector:
              matchLabels:
                app.kubernetes.io/name: cosmosigner
                app.kubernetes.io/instance: mychain-signer
            topologyKey: kubernetes.io/hostname
```

Required anti-affinity across nodes leaves replicas Pending when the cluster has fewer nodes than
replicas, including a single-node cluster. Preferred anti-affinity permits co-location on fewer
nodes. Changing either scheduling field on a running signer uses the same break-before-make
migration as an image or resource change: all replicas stop and restart once, retaining their PVCs
and Raft state. Cosmopilot does not validate these fields: a value Kubernetes rejects, or one no
node satisfies, keeps the whole signer down after the stop until the spec is corrected. With both fields unset, this feature does not change the lifecycle
digest or restart existing signers on operator upgrade.

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

## Migrating from TmKMS on 4.x

Complete this migration while still running Cosmopilot **4.x**, before applying the 5.0.0 CRDs or
operator. See the [5.0.0 upgrade guide](../getting-started/upgrading-to-5.md).

`Cosmosigner`'s Vault backend can point at the **same transit key** a `TmKMS` validator already uses.
To migrate, remove the `.spec.validator.tmKMS` block and add an equivalent `.spec.cosmosigner` block
with `backend.vault.keyName` set to the same key. No key material is moved. `Cosmopilot` removes the
`TmKMS` sidecars and deploys the signer `StatefulSet`. Wait until signing resumes, no node Pod has
a `tmkms` container, and no owned `<name>-tmkms` ConfigMap remains. Version 5.0.0 refuses either
legacy artifact instead of falling back to local-key signing.

A TmKMS token reused as-is lacks the cluster-binding registry permissions. First create the KV v2
registry mount described in [Vault Transit](#vault-transit), then add
`cosmosigner/data/cluster-bindings/*` with `create`, `update`, and `read` (or grant it `read` and use a
separate `claimTokenSecret` for `create` and `update`) and
`cosmosigner/metadata/cluster-bindings/*` with `read`. Its existing `lookup-self` and `renew-self`
grants carry over.

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

During this window the targeted nodes wait in `wait-cosmosigner-discovery` until a signer reaches
them. If the signer remains unavailable for 25 seconds, the gate fails with DNS diagnostics and the
controller recreates the Pod. The app starts after the gate receives signer data.

### What a first rollout looks like

Nodes and their signer start together. Nodes wait in their final startup gate while the signer may
log `resolve target nodes … no such host` until discovery DNS records appear. Once the signer reaches
the gate, it closes the connection, the app binds its privval port, and the signer redials directly.
Cosmosigner 3.1.0 bounds redial so late listeners and replaced node IPs can converge. Repeated
`can't get pubkey` app exits after the gate passes indicate a problem to investigate, rather than
normal startup behaviour.

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
Raft state. Independent claims inside one `ChainNodeSet` are also rejected, while a local-to-Cosmosigner
migration for the same logical validator shares one claim. This closes the cross-resource
and same-root double-sign windows during migrations and upgrades.

Helm installs files from a chart's `crds/` directory on first install, but does not upgrade or add them
on `helm upgrade`. Existing installations must apply the CRDs from the target chart before upgrading
the controller:

```shell
helm show crds oci://ghcr.io/voluzi/helm/cosmopilot --version <target-version> | kubectl apply --server-side --force-conflicts -f -
```

Confirm `consensuskeyreservations.cosmopilot.voluzi.com` exists before starting the new controller.
Without it, reservation-aware reconciliation fails closed: new signing paths are not created, but
already-running validators may remain online until the CRD is installed.

Do not change a validator signing configuration while old and new Cosmopilot controller versions are
running together during a rolling operator upgrade. Reservations are atomic among reservation-aware
controllers, but an older controller does not consult them. Apply the CRD, finish the controller
rollout, and only then begin a local-to-Cosmosigner migration.

Reservations are released automatically by a dedicated finalizer on the owning `ChainNode` or
`ChainNodeSet`. Cosmopilot first prevents the retired claim from being recreated, requests deletion
of its managed local-validator and Cosmosigner workloads, and waits until the relevant
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
not by changing the managed signer backend. A backend change that resolves to a different key is
refused before anything is torn down: the running signer keeps signing with its recorded key, and the
refusal is reported as a reconcile error and a `Warning` event until the change is reverted. The
exception is a key that another signer in the same `ChainNodeSet` also selects, or whose consensus-key
reservation is held elsewhere: those conflicts still stop every signer that may serve the key.
:::

:::warning[Slash-protection state at implementation boundaries]
Break-before-make prevents two signing implementations from running concurrently, and Cosmosigner
retains its Raft high-water mark across same-key Cosmosigner upgrades. It cannot import historical
`priv_validator_state.json` from a local validator. Before the first migration into
Cosmosigner, stop the old path cleanly, ensure the validator data cannot roll back below the last
signed height, and retain the old signing state for incident recovery. A public-key match alone does
not transfer slash-protection history, so Cosmopilot refuses to remove a validator-serving
Cosmosigner back to an independent local engine. That handoff requires a future explicit
quiesce, slash-state transfer, and verification protocol.
:::
