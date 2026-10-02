# Upgrading from 4.x to 5.0.0

Cosmopilot 5.0.0 removes tmKMS and connects Cosmosigner directly to the node's privval listener.
Complete the steps below in order. Do not change signing configurations while old and new operator
versions are running together.

## 1. Migrate legacy validators on 4.x

While still running Cosmopilot 4.x, migrate every tmKMS validator to
[Cosmosigner](../usage/cosmosigner.md#migrating-from-tmkms-on-4x). For Vault, use the same Transit key
and grant the token the Cosmosigner cluster-binding registry permissions. Wait for the signer to
roll out and confirm blocks are being signed with the existing validator public key.

Before continuing, confirm that every migrated node Pod has no `tmkms` container and that its owned
`<name>-tmkms` ConfigMap and tmKMS identity Secret have been removed. Preserve the old signing state for recovery: switching
implementations does not transfer tmKMS slash-protection history into Cosmosigner's Raft store.

The 5.0.0 CRDs remove `validator.tmKMS`, node-group `validator.tmKMS`, and the legacy reservation
status field. Kubernetes prunes these fields. To prevent a former tmKMS validator from accidentally
signing with a retained local key, the new controller refuses reconciliation with a Warning event
when a `tmkms` container, an owned `<name>-tmkms` ConfigMap, or a recognized `<name>-tmkms`
identity Secret remains. The identity Secret has no owner reference and survives owner deletion.
The guard recognizes its root attribution or the exact unstamped legacy Secret shape. It leaves the node Pod,
keys, and status untouched. A ChainNodeSet is also refused before reconciliation when an owned
child has any of these artifacts. Do not delete these artifacts to bypass an unfinished migration;
finish the migration on 4.x first. A Pod with a `tmkms` container must be retired by completing
that migration or deleted manually. Deleting its ConfigMap alone does not
retire the Pod. If the node already migrated to Cosmosigner and only a stale `<name>-tmkms`
ConfigMap or identity Secret remains, verify that Cosmosigner is signing with the existing
validator public key, then delete the stale ConfigMap or identity Secret.

If all three artifacts are already gone and no Cosmosigner is configured, the guard cannot fire. After
CRD pruning, the former tmKMS validator is treated as a local-key validator: it would sign from a
retained `<name>-priv-key` Secret, or get a new consensus key generated if none exists. A retained
key does not carry tmKMS's slash-protection history, and a new key does not match the validator's
on-chain identity. Before upgrading in this case, check the original 4.x signing configuration or
saved manifests, identify the consensus public key registered on-chain, and verify which backend
holds that key and its signing history. Keep the validator stopped and preserve its key and signing
state while restoring or completing the migration to Cosmosigner on 4.x. Upgrade only after
independently confirming Cosmosigner is signing with the same public key; absence of the artifacts
is not proof that migration completed.

## 2. Update image overrides and Helm values

Remove `tmkmsImage` and `vaultTokenRenewerImage` from your Helm values and remove the corresponding
manager flags or environment variables in custom deployments. The `vault-token-renewer` binary,
image, and workflow are removed; Cosmosigner renews Vault tokens internally.

Remove older `nodeUtilsImage` and `cosmosignerImage` pins, including per-signer `.image` overrides,
or update them to compatible images:

- **node-utils 4.0.0 or newer** supplies the `wait-for-signer` init command.
- **Cosmosigner 3.1.0 or newer** supplies HTTP health endpoints and bounded redial.

The release defaults use these versions. Cosmosigner semantic version tags older than 3.1.0 are
refused in preflight before replacing a running signer. Moving tags and digest-only references
cannot be version-checked locally and are allowed so unreleased builds can be tested; verify their
contents before upgrading. If an `edge`, `latest`, or digest-only reference actually contains a
build older than 3.1.0, `/livez` never answers, the signer never becomes live, and the validator does
not sign until the image is corrected. For immutable images, a version tag plus digest also permits
the version check.

## 3. Verify NetworkPolicy enforcement

Your CNI must enforce NetworkPolicy and support `endPort`. The new target-node policy allows TCP
26659 only from the associated signer pods in the same namespace. It allows all other TCP ports,
plus all UDP and SCTP ports, from anywhere so P2P, RPC, and other node traffic remain reachable.
Other NetworkPolicies are additive: ensure no policy also opens TCP 26659 to unrelated pods.
Without enforcement, privval is open cluster-wide; its SecretConnection does not authenticate the
signer.

## 4. Apply CRDs, then upgrade the operator

Helm installs CRDs on first installation but does not update them on `helm upgrade`. Apply the CRDs
from the target chart before upgrading the controller. Replace `<target-chart-version>` with the
chart release for Cosmopilot 5.0.0 and `<release>` with your existing release name:

```shell
helm show crds oci://ghcr.io/voluzi/helm/cosmopilot --version <target-chart-version> | kubectl apply -f -
helm upgrade <release> oci://ghcr.io/voluzi/helm/cosmopilot --version <target-chart-version> -f values.yaml
```

Confirm that `consensuskeyreservations.cosmopilot.voluzi.com` exists and wait for the operator rollout
to finish before making further signer configuration changes.

## 5. Observe the rollout

Expect every ChainNode Pod to be recreated, including full nodes without a signer: the node-utils
image bump and removal of the `TMKMS_PROXY` environment variable change every Pod's spec hash.
With disruption checks enabled, replacements are serialized by disruption locks within their
disruption domains. Each managed signer performs one break-before-make migration. Existing same-key
Raft PVCs retain the high-water mark. There may be brief missed blocks; rehearse this upgrade before
applying it to production validators.

The target's final `wait-cosmosigner-discovery` init container now listens on TCP 26659. It reads at
least one byte from a signer connection, closes it, and exits. The app binds the same port and the
signer redials directly. The node-utils sidecar continues its other duties. A gate that cannot
confirm a connection within 25 seconds fails with DNS diagnostics, and the controller recreates
the node Pod.

During a ChainNodeSet signer migration, a child Pod can be recreated while its parent-managed
signer is still quiesced. With no signer available to connect, the 25-second gate fails, the Pod
goes `Failed`, and the controller recreates it. This cycle can repeat until the signer is back.
These repeated child Pod failures and recreations are expected migration churn; observe the
parent's signer migration progress before treating them as a separate fault.

Signer startup and liveness probes use HTTP `/livez`; readiness uses `/readyz` on port 8080. Followers
pass readiness after backend and preflight initialization too. No Service exposes this HTTP port.
Confirm that signing resumes, signer replicas are ready, and node P2P/RPC endpoints remain reachable.
