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

For `ChainNodeSet` node groups and Cosmoseed, automatic peer discovery includes only Services in the set's namespace. To connect a node group to peers in another namespace, configure its `peers` list explicitly with a reachable IP or DNS name (for an in-cluster Service, use its namespace-qualified Kubernetes FQDN). Cosmoseed has no effective manual cross-namespace peer override, so peers it needs through automatic discovery must have Services in the same namespace. Earlier cluster-wide discovery emitted bare Service names, which were not reliably resolvable across namespaces.

## Notes

- `allowNonRoutable` can be enabled for private networks or testing environments.
- If `ingress` is omitted, the seed nodes will not be reachable via HTTP.
