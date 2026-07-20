# Nibiru Testnet Multi Validator Cosmosigner

```yaml
# Multiple validators, each fronted by its own Cosmopilot-managed Cosmosigner. A per-group
# `cosmosigner` block deploys one remote signer per validator group, and each signer holds ONE
# consensus identity. To run several validators in one ChainNodeSet, declare one group per
# validator, each with its own cosmosigner block and its own key — the top-level
# `.spec.cosmosigner` only ever fronts one validator identity. A validator group may also run
# MULTIPLE instances (see validator-c below): they are redundant signing endpoints of the SAME
# validator (HA), never extra validators.
apiVersion: cosmopilot.voluzi.com/v1
kind: ChainNodeSet
metadata:
  name: nibiru-testnet
spec:
  app:
    image: ghcr.io/nibiruchain/nibiru
    version: 2.9.0
    app: nibid
    sdkVersion: v0.47

  genesis:
    # Placeholder — replace with the real genesis URL of the network you are joining. Each signer's
    # consensus key (the Vault transit keys below) must already belong to that network's validator set.
    url: https://replace-me.example/nibiru-testnet-0/genesis.json

  nodes:
    # A single-instance validator group with its own signer. The signer resource is named
    # "<nodeset>-<group>-signer" (here "nibiru-testnet-validator-a-signer") and uses the Vault key
    # named below. The consensus key lives in Vault, so no local priv-key secret is referenced or
    # mounted — the validator signs exclusively through the signer.
    - name: validator-a
      instances: 1
      validator: {}
      cosmosigner:
        replicas: 3
        raftTLSSecret: nibiru-testnet-validator-a-cosmosigner-raft-tls
        backend:
          vault:
            address: https://vault.vault-system.svc.cluster.local:8200
            keyName: nibiru-testnet-validator-a
            keyVersion: 1
            tokenSecret:
              name: vault-cosmosigner-token
              key: token
            certificateSecret:
              name: vault-ca
              key: ca.crt

    # A second single-instance validator group with its own, DISTINCT signer key. Two signers must
    # never reference the same Vault/GCP key or the same software key secret — the webhook rejects it
    # (double-signing).
    - name: validator-b
      instances: 1
      validator: {}
      cosmosigner:
        replicas: 3
        raftTLSSecret: nibiru-testnet-validator-b-cosmosigner-raft-tls
        backend:
          vault:
            address: https://vault.vault-system.svc.cluster.local:8200
            keyName: nibiru-testnet-validator-b
            keyVersion: 1
            tokenSecret:
              name: vault-cosmosigner-token
              key: token
            certificateSecret:
              name: vault-ca
              key: ca.crt

    # A third validator, this time with THREE instances: still ONE validator (one signer, one Vault
    # key). The signer dials all three pods as redundant signing endpoints — the raft leader
    # produces exactly one signature per height and every node can relay it, so the validator
    # stays live through node restarts (HA). Adding more VALIDATORS means adding more groups;
    # adding more INSTANCES to a signed group only adds redundancy.
    - name: validator-c
      instances: 3
      validator:
        config:
          override:
            app.toml:
              minimum-gas-prices: 0.025unibi
      cosmosigner:
        replicas: 3
        raftTLSSecret: nibiru-testnet-validator-c-cosmosigner-raft-tls
        backend:
          vault:
            address: https://vault.vault-system.svc.cluster.local:8200
            keyName: nibiru-testnet-validator-c
            keyVersion: 1
            tokenSecret:
              name: vault-cosmosigner-token
              key: token
            certificateSecret:
              name: vault-ca
              key: ca.crt

    # Regular (non-validator) full nodes.
    - name: fullnodes
      instances: 2
      config:
        override:
          app.toml:
            minimum-gas-prices: 0.025unibi

```
