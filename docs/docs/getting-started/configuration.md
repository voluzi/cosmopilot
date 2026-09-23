# Configuration

This page describes all Helm configuration options available for installing and customizing `Cosmopilot`.
These settings allow you to tailor the deployment to your specific needs.

You can find the full list of available Helm values [here](https://github.com/voluzi/cosmopilot/blob/main/helm/cosmopilot/values.yaml), or you can run:

```bash
$ helm show values oci://ghcr.io/voluzi/helm/cosmopilot
```


## **General Settings**

### `replicas`
- **Description**: Number of replicas for the Cosmopilot deployment. If more than one, leader election will be enabled.
- **Default**: `1`

### `probesEnabled`
- **Description**: Enable or disable health and readiness probes for `cosmopilot` deployment.
- **Default**: `true`

### `image`
- **Description**: The container image repository for the Cosmopilot operator.
- **Default**: `ghcr.io/voluzi/cosmopilot`

### `imageTag`
- **Description**: The tag with the version to be used.
- **Default**: The chart's application version.

The companion-image values below are optional overrides. Their Helm defaults are empty, so the
selected manager release supplies its pinned, compatible image defaults. Upgrading the manager can
therefore upgrade its companions as one release. Explicit values remain pinned across upgrades,
including upgrades that use `--reuse-values`; clear or delete an old value to resume release defaults.

### `nodeUtilsImage`
- **Description**: The container image of `node-utils` (with version tag included). This is a container deployed by `cosmopilot` as a sidecar with helper methods for calculating data size, handling upgrades, and a few more utilities.
- **Default**: `""` (inherits the pinned default from the selected manager release)

:::warning[node-utils compatibility]
If you override or pin `nodeUtilsImage`, use node-utils 3.0.0 or newer. This version provides the
polling and SDK marker-based upgrade coordination required by Pods that do not use a trace FIFO.
The same release also validates custom app and Pod security contexts: when both are supplied, their
effective combination must set numeric `runAsUser` and `runAsGroup` values so node-utils can read
the SDK marker with the application's filesystem identity.
:::

### `cosmoGuardImage`
- **Description**: The container image of [CosmoGuard](https://github.com/voluzi/cosmoguard) (with version tag included) used for the standalone CosmoGuard deployments.
- **Default**: `""` (inherits the pinned default from the selected manager release)

### `cosmoseedImage`
- **Description**: The container image of [Cosmoseed](https://github.com/voluzi/cosmoseed) (with version tag included). Used when deploying seed nodes.
- **Default**: `""` (inherits the pinned default from the selected manager release)

### `cosmosignerImage`
- **Description**: The default container image of [Cosmosigner](https://github.com/voluzi/cosmosigner) (with version tag included), used when deploying managed remote signers. Can be overridden per-signer with `.spec.cosmosigner.image`.
- **Default**: `""` (inherits the pinned default from the selected manager release)

### `dataExporterImage`
- **Description**: The container image of Data Exporter (with version tag included) used by snapshot tarball upload and deletion Jobs.
- **Default**: `""` (inherits the pinned default from the selected manager release)

### `utilityImage`
- **Description**: Versioned utility image used by operator-owned helper containers for genesis, configuration, PVC, and snapshot integrity operations. Overrides may use a tag or digest and must provide the commands used by these helpers, including standard file utilities, `jq`, `wget`, `gunzip`, `zstd`, `pidof`, and `nc`.
- **Default**: `""` (inherits the pinned default from the selected manager release)

### `tmkmsImage`
- **Description**: TmKMS image used by the deprecated TmKMS validator sidecar and its identity/upload helper Pods.
- **Default**: `""` (inherits the pinned default from the selected manager release)

### `vaultTokenRenewerImage`
- **Description**: Image for the deprecated Vault token-renewer sidecar used only when a legacy TmKMS configuration enables `autoRenewToken`.
- **Default**: `""` (inherits the pinned default from the selected manager release)

### `imagePullSecrets`
- **Description**: Secrets for pulling the Cosmopilot manager image. Helper Pods run in the `ChainNode` namespace and use namespace-local secrets from `.spec.config.imagePullSecrets` instead.
- **Default**: `[]`

## **Worker Configuration**

### `workerCount`
- **Description**: Maximum number of concurrent reconciles handled by the `cosmopilot` operator.
- **Default**: `10`

### `workerName`
- **Description**: Name of the worker. Useful if you need multiple installations of `cosmopilot`. You can later define on resources which worker to use.
- **Default**: `""` (empty string)

## **Features and Functionality**

### `webHooksEnabled`
- **Description**: Enable or disable admission webhooks for validating and mutating requests. Ensure [cert-manager](https://cert-manager.io/docs/) is installed before enabling this.
- **Default**: `true`

### `debugMode`
- **Description**: Enable debug mode for additional logs.
- **Default**: `false`

### `disruptionChecksEnabled`
- **Description**: Enable or disable the operator's disruption checks during managed pod replacement.
- **Default**: `true`

### `disruptionMaxUnavailable`
- **Description**: Maximum number of unavailable pods allowed in a disruption domain before the operator defers another ready pod's replacement. This is a global setting for all ChainNodes, including validators; it must be at least `1`. A syncing pod counts as unavailable because it is not serving traffic.
- **Default**: `1`

## **Pod Priority Settings**

### `nodesPodPriority`
- **Description**: Priority for pods representing blockchain nodes.
- **Default**: `950`

### `validatorPodPriority`
- **Description**: Priority for validator pods.
- **Default**: `1050`

### `defaultPriority`
- **Description**: Default pod priority for all pods without specific roles.
- **Default**: `0`

## **Node Configuration**

### `nodeSelector`
- **Description**: Node selectors to control where `Cosmopilot` components are scheduled.
- **Default**: `{}` (no specific node selector)
