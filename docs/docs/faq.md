# FAQ

### What is Cosmopilot?

`Cosmopilot` is a Kubernetes operator that deploys and manages Cosmos-SDK-based
blockchain nodes. It automates node deployment, upgrades, disk resizing, API exposure,
backups (with integrity validation) and more. See the [Architecture](./reference/architecture)
overview for how it works.

### What chains are supported?

Any chain built on the Cosmos SDK that meets the required CLI commands and API
endpoints. Check the [Chain Compatibility](./getting-started/chain-compatibility) page
for the requirements and the list of tested chains.

### What's the difference between a ChainNode and a ChainNodeSet?

A `ChainNode` manages a single node. A `ChainNodeSet` manages multiple nodes organized
into groups (for example full nodes, sentries, validators) with shared genesis,
services and ingresses. A `ChainNodeSet` creates and owns `ChainNode` resources behind
the scenes. See [Deploy a Node](./usage/deploy-node) and
[Deploy a Node Set](./usage/deploy-node-set).

### Do I need cert-manager?

Only if you keep admission webhooks enabled (the default) or expose endpoints with TLS.
You can install without cert-manager by setting `webHooksEnabled=false`, at the cost of
the validation webhooks. See [Installation](./getting-started/installation).

### Is each node a StatefulSet?

No. Each `ChainNode` is backed by a single **Pod** plus its own PVC(s), Service and
configuration, which gives the operator fine-grained control over the node lifecycle.

### How are nodes upgraded?

`Cosmopilot` can monitor governance for upgrade proposals and upgrade nodes
automatically when the upgrade height is reached, and also supports manually scheduled
upgrades. See [Upgrades](./usage/upgrades).

### How does backup work?

`Cosmopilot` takes volume snapshots on a configurable schedule with retention
policies, can optionally verify their integrity by starting a throwaway node from the
snapshot, and can export snapshots as tarballs to external storage (GCS). See
[Persistence & Backup](./usage/persistence-and-backup).

### Can I expose my node's API publicly?

Yes — RPC, LCD, gRPC and EVM endpoints can be exposed via Ingress or Gateway API,
optionally fronted by [CosmoGuard](./usage/cosmoguard) for access control and caching.
See [Exposing Endpoints](./usage/exposing-endpoints).

### How do I monitor nodes?

Each node exposes Prometheus metrics on port `26660`. `Cosmopilot` does not create
`ServiceMonitor` resources for you — see [Monitoring & Observability](./usage/monitoring)
for a ready-to-use example.

### Can I run more than one operator instance?

Yes. You can shard work across multiple operator instances using `workerName`, and
control reconcile concurrency with `workerCount`. See
[Configuration](./getting-started/configuration#worker-configuration).

### How do I uninstall Cosmopilot?

See the [Uninstall](./getting-started/uninstall) guide. Note that removing the CRDs
deletes all `ChainNode`/`ChainNodeSet` resources, so plan accordingly.

### Where do I report bugs or ask for help?

Open an issue at
[github.com/voluzi/cosmopilot/issues](https://github.com/voluzi/cosmopilot/issues) or
email [dev@voluzi.com](mailto:dev@voluzi.com).
