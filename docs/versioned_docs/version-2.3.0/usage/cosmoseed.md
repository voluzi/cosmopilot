# Using Cosmoseed

`Cosmoseed` provides lightweight seed nodes for Cosmos networks. `Cosmopilot` can deploy and manage these seed nodes alongside your regular nodes.

## Enabling Cosmoseed

Enable Cosmoseed in a `ChainNodeSet` by configuring the `cosmoseed` section:

```yaml
cosmoseed:
  enabled: true
  instances: 2            # optional, defaults to 1
  allowNonRoutable: true  # optional, defaults to false
  expose:                 # optional P2P exposure
    p2p: true
    p2pServiceType: LoadBalancer
  ingress:                # optional HTTP ingress for monitoring
    host: seeds.example.com
    ingressClass: nginx   # optional, defaults to nginx
```

This configuration deploys two seed nodes, exposes their P2P ports and creates an ingress reachable at `seeds.example.com`.

## Notes

- `allowNonRoutable` can be enabled for private networks or testing environments.
- If `ingress` is omitted, the seed nodes will not be reachable via HTTP.

