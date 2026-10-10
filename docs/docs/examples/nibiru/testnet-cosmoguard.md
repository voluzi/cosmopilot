# Nibiru Testnet Cosmoguard

```yaml
# Local testnet with three guard replicas; rules are included below.
apiVersion: cosmopilot.voluzi.com/v1
kind: ChainNodeSet
metadata:
  name: nibiru-testnet
spec:
  app:
    image: ghcr.io/nibiruchain/nibiru
    version: 2.21.0
    app: nibid
    sdkVersion: v0.47

  validator:
    accountPrefix: nibi
    valPrefix: nibivaloper

    info:
      moniker: cosmopilot

    config:
      override:
        app.toml:
          minimum-gas-prices: 0.025unibi

    init:
      chainID: nibiru-testnet-0
      assets: ["100000000000000unibi", "1000000000000000000unusd", "10000000000000000uusdt"]
      stakeAmount: 100000000unibi
      unbondingTime: 60s
      votingPeriod: 60s
      additionalInitCommands:
        - command: [ "sh", "-c" ]
          args:
            - |
              nibid genesis add-sudo-root-account \
                $(nibid keys show account -a --home=/home/app --keyring-backend test) \
                --home=/home/app

  nodes:
    - name: fullnodes
      instances: 3

      config:
        override:
          app.toml:
            minimum-gas-prices: 0.025unibi

        cosmoGuard:
          enable: true
          config:
            name: cosmoguard-config
            key: cosmoguard.yaml

          replicas: 3
          # Alternative: autoscaling requires metrics-server.
          # autoscaling:
          #   enable: true
          #   minReplicas: 2
          #   maxReplicas: 8
          #   targetCPUUtilizationPercentage: 75

          # kubectl port-forward svc/nibiru-testnet-fullnodes-cg 8080:8080
          dashboard:
            enable: true
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cosmoguard-config
data:
  cosmoguard.yaml: |
    lcd:
      default: allow
    rpc:
      default: allow
      jsonrpc:
        default: allow
    grpc:
      default: allow

```
