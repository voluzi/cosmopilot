# Upgrades

This page explains how `Cosmopilot` handles upgrades for `ChainNode` and `ChainNodeSet` resources, including both **Governance** and **Manual** upgrades.

## Initial Version

The `.spec.app.image` and `.spec.app.version` fields specify the initial image and tag of the application for a `ChainNode` or all nodes in a `ChainNodeSet`. If no upgrades are configured, changing these fields will cause the node(s) to restart with the new image.

However, once upgrades are configured, these fields are ignored for the remainder of the node(s)' lifetime. At this point, all image changes are managed through the upgrade process.

## How the Running Image Is Resolved

The image a node runs is resolved with the following precedence:

1. `.spec.overrideImage`, if set — used verbatim.
2. `.spec.overrideVersion`, if set — a tag-only shorthand, applied to the repository in `.spec.app.image`.
3. The image of the highest upgrade the node has already reached — **used verbatim**.
4. `.spec.app.image` joined with `.spec.app.version`.

An upgrade image replaces the **whole** reference, not just the tag. This means an upgrade may move a node to a different registry or repository, which is exactly what a governance proposal does when a chain publishes a release somewhere new:

```yaml
app:
  image: alloranetwork/allora-chain   # initial repository
  version: v0.8.2
  upgrades:
  - height: 10511421
    image: registry.example.com/team/allorad:v0.17.1   # node moves to this repository
```

`.status.appImage` records the full image currently deployed, and `.status.appVersion` its tag or digest.

:::warning
Because the upgrade image is used verbatim, it should always carry an explicit tag or digest. An image without one resolves to `latest`, and `Cosmopilot` emits an admission warning for it.
:::

## Governance Upgrades

By default, `Cosmopilot` monitors the blockchain for on-chain governance upgrades. This feature can be disabled by setting `.spec.app.checkGovUpgrades` to `false`. 

Cosmos-SDK based chains often use governance proposals to manage upgrades, which include all necessary information, such as:
- **Upgrade Height**: The block height at which the upgrade should occur.
- **Binaries**: Links to the binaries for the new version.

For full automation, ensure that the governance proposal includes the **container image** (with the proper tag) under the key `docker`. When the image is provided, `Cosmopilot` performs the upgrade automatically without requiring manual intervention.

### Governance Upgrade Workflow
1. When an upgrade proposal passes, `Cosmopilot` adds the upgrade to `.status.upgrades` as `scheduled`.
2. If the container image is not included in the proposal, the upgrade is marked as `missing image`. In this case, you must manually add the upgrade to `.spec.app.upgrades` (see [Manual Upgrades](#manual-upgrades)).

## Manual Upgrades

Manual upgrades allow you to define upgrades directly in `.spec.app.upgrades`. These upgrades result in a straightforward binary swap, and `Cosmopilot` does not wait for the node to panic and halt, as is typical with governance upgrades.

### Adding a Manual Upgrade
Example configuration:
```yaml
app:
  upgrades:
  - height: 3000
    image: yourimage:yourtag
```

### Handling Governance Upgrades Without Images

If a governance upgrade does not include the required container image, you can manually add the upgrade and ensure it aligns with the governance proposal. In this case, set the `forceOnChain` field to `true`. This instructs `Cosmopilot` to treat the manual entry as part of the governance process.

Example configuration for missing image:
```yaml
app:
  upgrades:
  - height: 3000
    image: yourimage:yourtag
    forceOnChain: true # Optional. Use only for governance upgrades.
```

## Pinning a Node to a Specific Image

To hold a node on a given image regardless of upgrade history — to test a build, for example — set `.spec.overrideImage`:

```yaml
spec:
  overrideImage: registry.example.com/team/allorad:986-test
```

While this is set, `Cosmopilot` will not upgrade the node nor derive its image from upgrade history. `.spec.overrideVersion` does the same but only replaces the tag, keeping the repository from `.spec.app.image`; the two are mutually exclusive.

On a `ChainNodeSet`, both fields are available per node group (`.spec.nodes[].overrideImage`) and for the validator (`.spec.validator.overrideImage`). To unset either, remove it from the `ChainNodeSet` **and** from each `ChainNode` individually.

:::tip[Summary of Key Points]
- `.spec.app.image` and `.spec.app.version` control the initial image, but are ignored once upgrades are configured.
- An upgrade image is used verbatim, so an upgrade can move a node to a different registry or repository.
- Governance upgrades are automatic if the proposal includes the necessary container image under the `docker` key.
- Manual upgrades provide a flexible way to apply updates directly through `.spec.app.upgrades`.
- Use the `forceOnChain` field to handle governance upgrades that lack required images.
- Use `.spec.overrideImage` to pin a node to an exact image, or `.spec.overrideVersion` to pin only the tag.
:::