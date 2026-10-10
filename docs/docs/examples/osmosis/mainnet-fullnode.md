# Osmosis Mainnet Fullnode

```yaml
apiVersion: cosmopilot.voluzi.com/v1
kind: ChainNodeSet
metadata:
  name: osmosis
spec:
  app:
    image: osmolabs/osmosis
    version: 31.0.0
    app: osmosisd
    sdkVersion: v0.50
    sdkOptions:
      genesisSubcommand: false

  genesis:
    url: https://github.com/osmosis-labs/networks/raw/main/osmosis-1/genesis.json
    chainID: osmosis-1
    useDataVolume: true

  nodes:
    - name: fullnodes
      instances: 1

      peers:
        # Polkachu Nodes
        - id: ade4d8bc8cbe014af6ebdf3cb7b1e9ad36f412c0
          seed: true
          address: seeds.polkachu.com
          port: 12556

      persistence:
        size: 250Gi
        initTimeout: 3h
        additionalVolumes:
          - name: wasm
            size: 1Gi
            path: /home/app/wasm
          - name: ibc-08-wasm
            size: 1Gi
            path: /home/app/ibc_08-wasm
        additionalInitCommands:
          - image: ghcr.io/voluzi/node-tools
            command: [ "sh" ]
            args:
              - "-c"
              - |
                set -eu
                set -o pipefail
                curl -fL --retry 3 "https://snapshots.kjnodes.com/osmosis/snapshot_latest.tar.lz4" | lz4 -dc | tar -xf - -C /home/app

      config:
        runFlags: [ "--reject-config-defaults=true" ]
        override:
          app.toml:
            minimum-gas-prices: 0.025uosmo

```
