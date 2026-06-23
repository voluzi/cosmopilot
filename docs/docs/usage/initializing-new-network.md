# Initializing a New Network

`Cosmopilot` can generate a new genesis to create an entirely new network, making it ideal for launching testnets. This is achieved by configuring `.spec.validator.init` in both `ChainNode` and `ChainNodeSet` resources.

:::warning[Important]
This feature is intended for testnets. Avoid using it to create production networks.
:::

## Basic Configuration

Refer to the [GenesisInitConfig](../reference/crds#genesisinitconfig) for details on all available fields. Here’s an example of a basic configuration:

```yaml
validator:
  init:
    chainID: my-testnet-1
    stakeAmount: "1000000unibi"
    assets:
      - "100000000unibi"
      - "500000000uusdt"
```

This configuration will create a new network with the ID `my-testnet-1` and a single validator with assets of `100000000unibi` and `500000000uusdt`, staking `1000000unibi`.


## Multiple Genesis Validators

A `ChainNodeSet` can initialize a new network with **more than one validator** in the genesis. Instead of the singleton `.spec.validator`, define a validator group under `.spec.nodes[]` and set `instances` to the number of validators you want:

```yaml {2-3}
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

Instance `0` of the group generates the genesis; the remaining instances are added to that same genesis as full validators. Each instance gets its **own consensus key and operator account** — Cosmopilot creates the per-instance secrets (`<nodeset>-<group>-<index>-priv-key` and `<nodeset>-<group>-<index>-account`) automatically, so every validator is in the initial validator set with a distinct key. All instances share the same `init` parameters (`assets`, `stakeAmount`, commission, …).

See the [Nibiru Testnet Multi Validator](../examples/nibiru/testnet-multi-validator) example for a complete manifest.

:::info
Because each instance must sign with its own key, a multi-instance validator group cannot set a shared `privateKeySecret`, `tmKMS`, or `init.accountMnemonicSecret`. The group name `validator` is reserved for the legacy `.spec.validator`; use any other name.
:::

:::warning[Immutable after creation]
Once the genesis exists, its validator set is fixed. You cannot add, remove, scale, or change the keys of a genesis (`init`) validator group afterwards. To add validators to an already-running chain, use a [create-validator](../usage/validator#automated-create-validator) group instead.
:::

## Providing Validator Info

When initializing a new network, you can provide additional validator details using the `.spec.validator.info` field (or `.spec.nodes[].validator.info` for a group):

```yaml {2-4}
validator:
  info:
    moniker: nibiru
    website: https://nibiru.fi
  init:
    chainID: my-testnet-1
    stakeAmount: "1000000unibi"
    assets:
      - "100000000unibi"
      - "500000000uusdt"
```

## Account Prefixes

By default, `Cosmopilot` uses the `cosmos` prefix for accounts. If the network requires different prefixes, you can customize them:

```yaml {2-3}
validator:
  accountPrefix: cosmos
  valPrefix: cosmosvaloper
  init:
    chainID: my-testnet-1
    stakeAmount: "1000000uatom"
    assets: ["100000000uatom"]
```

:::info
The `accountPrefix` and `valPrefix` fields should be set directly under `.validator` as shown above. While these fields also exist under `.validator.init` and `.validator.createValidator` for backward compatibility, those locations are deprecated.
:::

## Additional Accounts

To fund other accounts, such as those for faucets or external tools, you can include them in the configuration:

```yaml {8-10}
validator:
  init:
    chainID: my-testnet-1
    stakeAmount: "1000000unibi"
    assets:
      - "100000000unibi"
      - "500000000uusdt"
    accounts:
      - address: nibi10jf963eruna3clq4c4jwsj3k4jf5snp3z3q372
        assets: ["100000000unibi", "500000000uusdt"]
```

Additionally, if you are adding accounts to be used by other `ChainNode`s (for other validators for example), you can use the account already created for them by `Cosmopilot`. For that, you can use:

```yaml {8-12}
validator:
  init:
    chainID: my-testnet-1
    stakeAmount: "1000000unibi"
    assets:
      - "100000000unibi"
      - "500000000uusdt"
    chainNodeAccounts:
      - chainNode: my-testnet-1-validator-2
        assets: [ "100000000000000unibi", "10000000000000000uusdt" ]
      - chainNode: my-testnet-1-validator-3
        assets: [ "100000000000000unibi", "10000000000000000uusdt" ]
```

:::tip[NOTE]
`Cosmopilot` will wait for the referenced `ChainNode` resources to exist and have their accounts ready before proceeding with the initialization.
:::

## Voting and Bonding Periods

For testnets, it is common to use shorter bonding and voting periods for faster testing. You can configure these as follows:

```yaml {5,6}
validator:
  init:
    chainID: my-testnet-1
    stakeAmount: "1000000unibi"
    unbondingTime: 60s
    votingPeriod: 60s
    assets:
      - "100000000unibi"
      - "500000000uusdt"
```

### Expedited Voting Period

For governance proposals that require faster resolution, you can also configure an expedited voting period. This is only supported on chains using SDK v0.50 or later:

```yaml {7}
validator:
  init:
    chainID: my-testnet-1
    stakeAmount: "1000000unibi"
    unbondingTime: 60s
    votingPeriod: 60s
    expeditedVotingPeriod: 30s
    assets:
      - "100000000unibi"
```

## Custom Genesis Changes

For more advanced genesis customizations, you can use the `.spec.validator.init.additionalInitCommands` field to execute additional initialization steps using custom containers.

:::info[Available Mounts]
- **`/home/app`**: Application home directory. The genesis file is typically located at `/home/app/config/genesis.json`. The validator account is available on the `test` keyring as `account`.
- **`/temp`**: Temporary volume shared between initialization containers, useful for data sharing.
:::

### Example: Adding a Sudo Root Account

On a [Nibiru](https://nibiru.fi) network, you can add a sudo root account to the genesis using:

```yaml {8-15}
validator:
  init:
    chainID: my-testnet-1
    stakeAmount: "1000000unibi"
    assets:
      - "100000000unibi"
      - "500000000uusdt"
    additionalInitCommands:
      - image: ghcr.io/nibiruchain/nibiru:1.5.0 # optional, defaults to the image use by the ChainNode.
        command: [ "sh", "-c" ] # optional, defaults to image entrypoint.
        args:
          - >
            nibid genesis add-sudo-root-account \
              $(nibid keys show account -a --home=/home/app --keyring-backend test) \
              --home=/home/app
```