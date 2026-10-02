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
`<name>-tmkms` ConfigMap has been removed. Preserve the old signing state for recovery: switching
implementations does not transfer tmKMS slash-protection history into Cosmosigner's Raft store.

The 5.0.0 CRDs remove `validator.tmKMS`, node-group `validator.tmKMS`, and the legacy reservation
status field. Kubernetes prunes these fields. To prevent a former tmKMS validator from accidentally
signing with a retained local key, the new controller refuses reconciliation with a Warning event
when a live `tmkms` container or an owned `<name>-tmkms` ConfigMap remains. It leaves the node Pod
alone. Do not bypass this guard by deleting the artifacts; finish the migration on 4.x first.

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
cannot be version-checked locally; verify their contents before upgrading. For immutable images,
a version tag plus digest also permits the version check.

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

Expect each signer-target Pod to be recreated once and each managed signer to perform one
break-before-make migration. Existing same-key Raft PVCs retain the high-water mark. There may be
brief missed blocks; rehearse this upgrade before applying it to production validators.

The target's final `wait-cosmosigner-discovery` init container now listens on TCP 26659. It reads at
least one byte from a signer connection, closes it, and exits. The app binds the same port and the
signer redials directly. The node-utils sidecar continues its other duties. A gate that cannot
confirm a connection within 25 seconds fails with DNS diagnostics, and the controller recreates
the node Pod.

Signer startup and liveness probes use HTTP `/livez`; readiness uses `/readyz` on port 8080. Followers
pass readiness after backend and preflight initialization too. No Service exposes this HTTP port.
Confirm that signing resumes, signer replicas are ready, and node P2P/RPC endpoints remain reachable.
