# Validator

This page explains how to configure a `ChainNode` or `ChainNodeSet` to run a validator. `Cosmopilot` provides multiple options to suit different use cases.

The options below (existing key, create-validator, genesis init) apply to a single validator configured under `.spec.validator`. A `ChainNodeSet` can also run **multiple validators** by declaring validator groups under `.spec.nodes[]` — see [Multiple Validators](#multiple-validators).

## Existing Consensus Key

::: warning Important
Storing mnemonics and private keys in Kubernetes secrets may not be secure and is recommended only for testnets. For production networks, consider using [TmKMS](11-tmkms) for enhanced security.
:::

If a consensus key is already defined in the genesis file as a validator, you can configure the node as follows:

1. Create a Kubernetes secret containing the private key:
```bash
$ kubectl create secret generic my-validator-key --from-file=my-validator-key.json
```
Make sure the secret is created in the same namespace as your `ChainNode` or `ChainNodeSet`.

2. Specify the secret name in `.spec.validator.privateKeySecret`.

Example configuration:
```yaml
validator:
  privateKeySecret: my-validator-key
```

## Manual Create-Validator

For nodes that are not validators by default, you can:
1. Configure `.spec.validator.info.moniker` with the validator's moniker.
2. Manually submit a `create-validator` transaction to the blockchain to make the node a validator.

Example configuration:
```yaml
validator:
  info:
    moniker: my-validator
```

## Automated Create-Validator

`Cosmopilot` can also submit a `create-validator` transaction for you. For that you need to:
1. Either create a Kubernetes secret containing the mnemonic of an account that has funds (the secret name must follow the pattern `<chainnode-name>-account`), or wait for `Cosmopilot` to create an account and you can send funds to it later.
2. Configure `.spec.validator.info` and `.spec.validator.createValidator`.

Example configuration:
```yaml
validator:
  info:
    moniker: my-validator
    website: https://validator.example.com
    details: "A reliable validator"
    identity: "ABCD1234"

  createValidator:
    stakeAmount: "1000000unibi"
    commissionRate: "0.1"
    commissionMaxRate: "0.2"
    commissionMaxChangeRate: "0.01"
    gasPrices: "0.025unibi"
    minSelfDelegation: "1"
```

## Multiple Validators

The `.spec.validator` field configures a single validator. To run **several validators in one `ChainNodeSet`**, declare validator groups under `.spec.nodes[]`: a group is a validator group when it has a `validator` block, and `instances` controls how many validators it runs. Each instance gets its **own consensus key and operator account**, created automatically by `Cosmopilot` (`<nodeset>-<group>-<index>-priv-key` and `<nodeset>-<group>-<index>-account`).

There are two ways to add validators, depending on whether you are bootstrapping a new network or joining an existing one.

### Genesis validators

Use a group with `validator.init` to bake multiple validators into a brand-new genesis. Instance `0` generates the genesis and the rest are added to it as full validators:

```yaml
nodes:
  - name: validators
    instances: 3
    validator:
      accountPrefix: nibi
      valPrefix: nibivaloper
      init:
        chainID: my-testnet-1
        assets: ["100000000unibi"]
        stakeAmount: "1000000unibi"
```

See [Initializing a New Network → Multiple Genesis Validators](10-initializing-new-network#multiple-genesis-validators) and the [Nibiru Testnet Multi Validator](/04-examples/nibiru/testnet-multi-validator) example.

### Validators on a running chain

Use a group with `validator.createValidator` (together with an external `.spec.genesis`) to add validators to a chain that is already running. Each instance submits its own `create-validator` transaction once it is synced:

```yaml
genesis:
  url: https://example.com/my-network-genesis.json
  chainID: my-network-1

nodes:
  - name: validators
    instances: 2
    validator:
      accountPrefix: nibi
      valPrefix: nibivaloper
      createValidator:
        stakeAmount: "1000000unibi"
        gasPrices: "0.025unibi"
        # accountMnemonicSecret: my-funded-account  # optional; otherwise an account is generated per instance and must be funded
```

### Things to know

- **Per-instance keys are mandatory.** A multi-instance validator group cannot set a shared `privateKeySecret`, `tmKMS`, or account secret — that would make every instance sign with the same key (double-signing). Single-instance validator groups may use them, just like `.spec.validator`.
- **`validator` is a reserved group name** (it backs the legacy `.spec.validator`). Use any other name for your groups.
- **Genesis (`init`) validator groups are immutable after the genesis is created** — you cannot add, remove, scale, or change their keys. Use a `createValidator` group to grow a running validator set.
- **Backward compatible.** The singleton `.spec.validator` continues to work unchanged and is equivalent to a one-instance validator group that keeps the legacy ChainNode name `<nodeset>-validator`.

## Initializing a New Network

Please refer to [Initializing a New Network](10-initializing-new-network) page for information about setting up new networks.