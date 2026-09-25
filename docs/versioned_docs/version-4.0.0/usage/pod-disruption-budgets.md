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

`ignoreGroupOnDisruptionChecks` widens the operator's managed pod replacement check by excluding the group label from its peer selection. It does not change the Kubernetes PDB selector or `minAvailable`.

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

- The operator's managed replacement check is separate from Kubernetes PDBs. Configure its global limit with Helm's `disruptionMaxUnavailable` (default `1`). There is no per-group controller budget in this release. Raising the global limit also increases permitted validator unavailability. Syncing pods count as unavailable by default; with `config.ignoreSyncing: true`, the app readiness probe uses `/health`, so a syncing pod may count as available. Other not-ready pods count as unavailable, and an already-unavailable pod may still be replaced for recovery. The operator defers a ready pod's replacement when the limit is exhausted and reports `PodRecreationDeferred` on the ChainNode.
- Replacement checks apply within a namespace and disruption domain. Validator pods on the same chain share a domain across nodesets and groups; non-validator domain labels follow `ignoreGroupOnDisruptionChecks`.
- PDBs are currently supported only on `ChainNodeSet` resources.
- `minAvailable` defaults to the number of instances minus one for node groups.
- On a node group with a `validator` block, use `nodes[].validator.pdb`. A group-level `nodes[].pdb` creates no PDB there.
- `ignoreGroupOnDisruptionChecks` has no effect on a validator group: validator pods already coordinate disruptions chain-wide, across every nodeset and group.
- A validator PDB only has an effect when multiple validators run in the same namespace; otherwise the default `minAvailable: 0` leaves it ineffective.
- During [upgrades](../usage/upgrades), PDBs are automatically disabled for `ChainNodes` with the `Upgrading` status.
