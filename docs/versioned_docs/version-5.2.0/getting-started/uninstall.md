# Uninstall

This guide explains how to cleanly remove `Cosmopilot` from your cluster and what
happens to your nodes and data.

:::warning[Important]
Deleting the Custom Resource Definitions (CRDs) deletes **all** `ChainNode` and
`ChainNodeSet` resources in the cluster, which in turn removes the Pods they manage.
Make sure you have backups/snapshots of any data you need before proceeding.
:::

## 1. Remove your nodes first (recommended)

Delete your `ChainNodeSet` and `ChainNode` resources so the operator can clean up the
resources it owns (Pods, Services, ingresses, ConfigMaps) in an orderly way:

```bash
kubectl delete chainnodeset --all -A
kubectl delete chainnode --all -A
```

This does **not** delete PVCs or volume snapshots by default, so your data is
preserved unless you remove it explicitly (see step 4).

## 2. Uninstall the Helm release

```bash
helm uninstall cosmopilot --namespace cosmopilot-system
```

This removes the manager Deployment, RBAC, webhook configuration and PriorityClasses
created by the chart.

## 3. Remove the CRDs (optional)

Helm does not remove CRDs automatically. Remove them only if you want to fully
uninstall `Cosmopilot` and you understand that this deletes any remaining
`ChainNode`/`ChainNodeSet` resources:

```bash
kubectl delete crd chainnodes.cosmopilot.voluzi.com
kubectl delete crd chainnodesets.cosmopilot.voluzi.com
```

:::tip
Run `kubectl get crd | grep cosmopilot` to confirm the exact CRD names installed in
your cluster before deleting them.
:::

## 4. Clean up data (optional)

PVCs and volume snapshots are intentionally left in place so you don't lose chain data
by accident. Remove them only when you're sure you no longer need them:

```bash
# Inspect first
kubectl get pvc -A | grep <chain>
kubectl get volumesnapshot -A

# Then delete what you no longer need
kubectl delete pvc <name> -n <namespace>
kubectl delete volumesnapshot <name> -n <namespace>
```

If you exported snapshot tarballs to external storage (for example GCS or S3), remove those
separately from your storage provider.

## 5. Namespace (optional)

If you created a dedicated namespace just for `Cosmopilot`:

```bash
kubectl delete namespace cosmopilot-system
```
