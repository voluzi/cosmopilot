# Pod Disruption Budgets

Pod Disruption Budgets (PDBs) ensure that a minimum number of pods remain available during voluntary disruptions such as node upgrades or evictions. `Cosmopilot` allows you to configure PDBs for validator and node groups within a `ChainNodeSet`.

## Examples

```yaml
spec:
  nodes:
    - name: fullnode
      instances: 3
      pdb:
        enabled: true
        minAvailable: 2   # optional, defaults to instances - 1
  validator:
    pdb:
      enabled: true
      minAvailable: 3    # meaningful only when other validators exist
```

With this configuration, Kubernetes will ensure that at least two fullnode pods remain running and, when multiple validators exist in the namespace, at least three validator pods stay available during maintenance operations.

### Validator node groups

A node group with a `validator` block is configured from that block, so its PDB goes under `nodes[].validator.pdb` — **not** `nodes[].pdb`, which is ignored on such a group (Cosmopilot emits an admission warning when you set it there):

```yaml
spec:
  nodes:
    - name: validators
      instances: 3
      validator:
        pdb:
          enabled: true
          minAvailable: 2   # optional, defaults to instances - 1
```

This creates a PDB named `<nodeset>-<group>-validator` selecting only that group's validator pods.

You may also instruct `Cosmopilot` to ignore group labels on PDB checks. This is useful to ensure no downtime globally or per global ingress, instead of just per group.

```yaml {12,17}
  ingresses:
  - name: fullnodes
    groups:
    - fullnode-a
    - fullnode-b

...

  nodes:
    - name: fullnode-a
      instances: 3
      ignoreGroupOnDisruptionChecks: true      
      pdb:
        enabled: true
    - name: fullnode-b
      instances: 3
      ignoreGroupOnDisruptionChecks: true      
      pdb:
        enabled: true
```            

## Notes

- PDBs are currently supported only on `ChainNodeSet` resources.
- `minAvailable` defaults to the number of instances minus one for node groups.
- On a node group with a `validator` block, use `nodes[].validator.pdb`. A group-level `nodes[].pdb` creates no PDB there.
- `ignoreGroupOnDisruptionChecks` has no effect on a validator group: validator pods already coordinate disruptions chain-wide, across every nodeset and group.
- A validator PDB only has an effect when multiple validators run in the same namespace; otherwise the default `minAvailable: 0` leaves it ineffective.
- During [upgrades](../usage/upgrades), PDBs are automatically disabled for `ChainNodes` with the `Upgrading` status.
