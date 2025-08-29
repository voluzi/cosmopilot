# Custom Resource Definitions (CRDs) API Reference

This page provides a detailed reference for the available Custom Resource Definitions (CRDs) in Cosmopilot. Each CRD defines the specifications and configuration options for managing Cosmos-based blockchain nodes in Kubernetes.
### Custom Resources

* [ChainNode](#chainnode)
* [ChainNodeSet](#chainnodeset)

### Sub Resources

* [ChainNodeList](#chainnodelist)
* [ChainNodeSpec](#chainnodespec)
* [ChainNodeStatus](#chainnodestatus)
* [ValidatorConfig](#validatorconfig)
* [ChainNodeSetList](#chainnodesetlist)
* [ChainNodeSetNodeStatus](#chainnodesetnodestatus)
* [ChainNodeSetSpec](#chainnodesetspec)
* [ChainNodeSetStatus](#chainnodesetstatus)
* [CosmoseedConfig](#cosmoseedconfig)
* [CosmoseedIngressConfig](#cosmoseedingressconfig)
* [GlobalIngressConfig](#globalingressconfig)
* [IndividualIngressConfig](#individualingressconfig)
* [IngressConfig](#ingressconfig)
* [NodeGroupSpec](#nodegroupspec)
* [NodeSetValidatorConfig](#nodesetvalidatorconfig)
* [PdbConfig](#pdbconfig)
* [SeedStatus](#seedstatus)
* [AccountAssets](#accountassets)
* [AppSpec](#appspec)
* [ChainNodeAssets](#chainnodeassets)
* [Config](#config)
* [CosmoGuardConfig](#cosmoguardconfig)
* [CreateValidatorConfig](#createvalidatorconfig)
* [ExportTarballConfig](#exporttarballconfig)
* [ExposeConfig](#exposeconfig)
* [FromNodeRPCConfig](#fromnoderpcconfig)
* [GcsExportConfig](#gcsexportconfig)
* [GenesisConfig](#genesisconfig)
* [GenesisInitConfig](#genesisinitconfig)
* [InitCommand](#initcommand)
* [Peer](#peer)
* [Persistence](#persistence)
* [PvcSnapshot](#pvcsnapshot)
* [SidecarSpec](#sidecarspec)
* [StateSyncConfig](#statesyncconfig)
* [TmKMS](#tmkms)
* [TmKmsHashicorpProvider](#tmkmshashicorpprovider)
* [TmKmsKeyFormat](#tmkmskeyformat)
* [TmKmsProvider](#tmkmsprovider)
* [Upgrade](#upgrade)
* [UpgradeSpec](#upgradespec)
* [ValidatorInfo](#validatorinfo)
* [VerticalAutoscalingConfig](#verticalautoscalingconfig)
* [VerticalAutoscalingMetricConfig](#verticalautoscalingmetricconfig)
* [VerticalAutoscalingRule](#verticalautoscalingrule)
* [VolumeSnapshotsConfig](#volumesnapshotsconfig)
* [VolumeSpec](#volumespec)

#### ChainNode

ChainNode is the Schema for the chainnodes API.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| metadata |  | metav1.ObjectMeta | false |
| spec |  | [ChainNodeSpec](#chainnodespec) | false |
| status |  | [ChainNodeStatus](#chainnodestatus) | false |

[Back to Custom Resources](#custom-resources)

#### ChainNodeList

ChainNodeList contains a list of ChainNode.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| metadata |  | metav1.ListMeta | false |
| items |  | [][ChainNode](#chainnode) | true |

[Back to Custom Resources](#custom-resources)

#### ChainNodeSpec

ChainNodeSpec defines the desired state of ChainNode.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| genesis | Indicates where this node will get the genesis from. Can be omitted when .spec.validator.init is specified. | *[GenesisConfig](#genesisconfig) | true |
| app | Specifies image, version and binary name of the chain application to run. It also allows to schedule upgrades, or setting/updating the image for an on-chain upgrade. | [AppSpec](#appspec) | true |
| config | Allows setting specific configurations for this node. | *[Config](#config) | false |
| persistence | Configures PVC for persisting data. Automated data snapshots can also be configured in this section. | *[Persistence](#persistence) | false |
| validator | Indicates this node is going to be a validator and allows configuring it. | *[ValidatorConfig](#validatorconfig) | false |
| autoDiscoverPeers | Ensures peers with same chain ID are connected with each other. Enabled by default. | *bool | false |
| stateSyncRestore | Configures this node to find a state-sync snapshot on the network and restore from it. This is disabled by default. | *bool | false |
| peers | Additional persistent peers that should be added to this node. | [][Peer](#peer) | false |
| expose | Allows exposing P2P traffic to public. | *[ExposeConfig](#exposeconfig) | false |
| resources | Compute Resources required by the app container. | corev1.ResourceRequirements | false |
| nodeSelector | Selector which must be true for the pod to fit on a node. Selector which must match a node's labels for the pod to be scheduled on that node. | map[string]string | false |
| affinity | If specified, the pod's scheduling constraints. | *corev1.Affinity | false |
| ignoreGroupOnDisruptionChecks | Whether ChainNodeSet group label should be ignored on pod disruption checks. This is useful to ensure no downtime globally or per global ingress, instead of just per group. Defaults to `false`. | *bool | false |
| vpa | Vertical Pod Autoscaling configuration for this node. | *[VerticalAutoscalingConfig](#verticalautoscalingconfig) | false |
| overrideVersion | OverrideVersion will force this node to use the specified version. NOTE: when this is set, cosmopilot will not upgrade the node, nor will set the version based on upgrade history. | *string | false |
| ingress | Indicates if an ingress should be created to access API endpoints of this node and configures it. | *[IngressConfig](#ingressconfig) | false |

[Back to Custom Resources](#custom-resources)

#### ChainNodeStatus

ChainNodeStatus defines the observed state of ChainNode

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| phase | Indicates the current phase for this ChainNode. | ChainNodePhase | false |
| conditions | Conditions to track state of the ChainNode. | []metav1.Condition | false |
| nodeID | Indicates this node's ID. | string | false |
| ip | Internal IP address of this node. | string | false |
| publicAddress | Public address for P2P when enabled. | string | false |
| chainID | Indicates the chain ID. | string | false |
| pvcSize | Current size of the data PVC for this node. | string | false |
| dataUsage | Usage percentage of data volume. | string | false |
| validator | Indicates if this node is a validator. | bool | true |
| accountAddress | Account address of this validator. Omitted when not a validator. | string | false |
| validatorAddress | Validator address is the valoper address of this validator. Omitted when not a validator. | string | false |
| jailed | Indicates if this validator is jailed. Always false if not a validator node. | bool | false |
| appVersion | Application version currently deployed. | string | false |
| latestHeight | Last height read on the node by cosmopilot. | int64 | false |
| seedMode | Indicates if this node is running with seed mode enabled. | bool | false |
| upgrades | All scheduled/completed upgrades performed by cosmopilot on this ChainNode. | [][Upgrade](#upgrade) | false |
| pubKey | Public key of the validator. | string | false |
| validatorStatus | Indicates the current status of validator if this node is one. | ValidatorStatus | false |

[Back to Custom Resources](#custom-resources)

#### ValidatorConfig

ValidatorConfig contains the configuration for running a node as validator.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| privateKeySecret | Indicates the secret containing the private key to be used by this validator. Defaults to `<chainnode>-priv-key`. Will be created if it does not exist. | *string | false |
| info | Contains information details about this validator. | *[ValidatorInfo](#validatorinfo) | false |
| init | Specifies configs and initialization commands for creating a new genesis. | *[GenesisInitConfig](#genesisinitconfig) | false |
| tmKMS | TmKMS configuration for signing commits for this validator. When configured, .spec.validator.privateKeySecret will not be mounted on the validator node. | *[TmKMS](#tmkms) | false |
| createValidator | Indicates that cosmopilot should run create-validator tx to make this node a validator. | *[CreateValidatorConfig](#createvalidatorconfig) | false |

[Back to Custom Resources](#custom-resources)

#### ChainNodeSet

ChainNodeSet is the Schema for the chainnodesets API.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| metadata |  | metav1.ObjectMeta | false |
| spec |  | [ChainNodeSetSpec](#chainnodesetspec) | false |
| status |  | [ChainNodeSetStatus](#chainnodesetstatus) | false |

[Back to Custom Resources](#custom-resources)

#### ChainNodeSetList

ChainNodeSetList contains a list of ChainNodeSet.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| metadata |  | metav1.ListMeta | false |
| items |  | [][ChainNodeSet](#chainnodeset) | true |

[Back to Custom Resources](#custom-resources)

#### ChainNodeSetNodeStatus

ChainNodeSetNodeStatus contains information about a node running on this ChainNodeSet.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| name | Name of the node. | string | true |
| public | Whether this node can be accessed publicly. | bool | true |
| seed | Indicates if this node is running in seed mode. | bool | true |
| id | ID of this node. | string | true |
| address | Hostname or IP address to reach this node internally. | string | true |
| publicAddress | Hostname or IP address to reach this node publicly. | string | false |
| publicPort | Port to reach this node publicly. | int | false |
| port | P2P port for connecting to this node. | int | true |
| group | Group to which this ChainNode belongs. | string | false |

[Back to Custom Resources](#custom-resources)

#### ChainNodeSetSpec

ChainNodeSetSpec defines the desired state of ChainNode.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| app | Specifies image, version and binary name of the chain application to run. It also allows to schedule upgrades, or setting/updating the image for an on-chain upgrade. | [AppSpec](#appspec) | true |
| genesis | Indicates where this node will get the genesis from. Can be omitted when .spec.validator.init is specified. | *[GenesisConfig](#genesisconfig) | true |
| validator | Indicates this node set will run a validator and allows configuring it. | *[NodeSetValidatorConfig](#nodesetvalidatorconfig) | false |
| nodes | List of groups of ChainNodes to be run. | [][NodeGroupSpec](#nodegroupspec) | true |
| ingresses | List of ingresses to create for this ChainNodeSet. This allows to create ingresses targeting multiple groups of nodes. | [][GlobalIngressConfig](#globalingressconfig) | false |
| cosmoseed | Allows deploying seed nodes using Cosmoseed. | *[CosmoseedConfig](#cosmoseedconfig) | false |

[Back to Custom Resources](#custom-resources)

#### ChainNodeSetStatus

ChainNodeSetStatus defines the observed state of ChainNodeSet.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| phase | Indicates the current phase for this ChainNodeSet. | ChainNodeSetPhase | false |
| chainID | Indicates the chain ID. | string | false |
| instances | Indicates the total number of ChainNode instances on this ChainNodeSet. | int | false |
| appVersion | The application version currently deployed. | string | false |
| nodes | Nodes available on this nodeset. Excludes validator node. | [][ChainNodeSetNodeStatus](#chainnodesetnodestatus) | false |
| validatorAddress | Validator address of the validator in this ChainNodeSet if one is available. Omitted when no validator is present in the ChainNodeSet. | string | false |
| validatorStatus | Current status of validator if this ChainNodeSet has one. | ValidatorStatus | false |
| pubKey | Public key of the validator if this ChainNodeSet has one. | string | false |
| upgrades | All scheduled or completed upgrades performed by cosmopilot on ChainNodes of this ChainNodeSet. | [][Upgrade](#upgrade) | false |
| latestHeight | Last height read on the nodes by cosmopilot. | int64 | false |
| seeds | Status of seed nodes (cosmoseed) | [][SeedStatus](#seedstatus) | false |

[Back to Custom Resources](#custom-resources)

#### CosmoseedConfig

CosmoseedConfig defines settings for deploying seed nodes via Cosmoseed.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| enabled | Whether to enable deployment of Cosmoseed. If false or unset, no seed node instances will be created. | *bool | false |
| instances | Number of seed node instances to deploy. Defaults to 1. | *int | false |
| expose | Configuration for exposing the P2P endpoint (e.g., via LoadBalancer or NodePort). | *[ExposeConfig](#exposeconfig) | false |
| resources | Compute Resources to be applied on the cosmoseed container. | corev1.ResourceRequirements | false |
| allowNonRoutable | Used to enforce strict routability rules for peer addresses. Set to false to only accept publicly routable IPs (recommended for public networks). Set to true to allow local/private IPs (e.g., in testnets or dev environments). Defaults to `false`. | *bool | false |
| maxInboundPeers | Maximum number of inbound P2P connections. Defaults to `2000`. | *int | false |
| maxOutboundPeers | Maximum number of outbound P2P connections. Defaults to `20`. | *int | false |
| peerQueueSize | Size of the internal peer queue used by dial workers in the PEX reactor. This queue holds peers to be dialed; dial workers consume from it. If the queue is full, new discovered peers may be discarded. Use together with `DialWorkers` to control peer discovery throughput. Defaults to `1000`. | *int | false |
| dialWorkers | Number of concurrent dialer workers used for outbound peer discovery. Each worker fetches peers from the queue (`PeerQueueSize`) and attempts to dial them. Higher values increase parallelism, but may increase CPU/network load. Defaults to `20`. | *int | false |
| maxPacketMsgPayloadSize | Maximum size (in bytes) of packet message payloads over P2P. Defaults to `1024`. | *int | false |
| additionalSeeds | Additional seed nodes to append to the node’s default seed list. Comma-separated list in the format `nodeID@ip:port`. | *string | false |
| logLevel | Log level of cosmoseed. Defaults to `info`. | *string | false |
| ingress | Ingress configuration for cosmoseed nodes. | *[CosmoseedIngressConfig](#cosmoseedingressconfig) | false |

[Back to Custom Resources](#custom-resources)

#### CosmoseedIngressConfig

CosmoseedIngressConfig configures ingress for cosmoseed nodes.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| host | Host in which cosmoseed nodes will be exposed. | string | true |
| annotations | Annotations to be appended to the ingress. | map[string]string | false |
| disableTLS | Whether to disable TLS on ingress resource. | bool | false |
| tlsSecretName | Name of the secret containing TLS certificate. | *string | false |
| ingressClass | IngressClass specifies the ingress class to be used on ingresses | *string | false |

[Back to Custom Resources](#custom-resources)

#### GlobalIngressConfig

GlobalIngressConfig specifies configurations for ingress to expose API endpoints of several groups of nodes.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| name | The name of this ingress. | string | true |
| groups | Groups of nodes to which this ingress will point to. | []string | true |
| enableRPC | Enable RPC endpoint. | bool | false |
| enableGRPC | Enable gRPC endpoint. | bool | false |
| enableLCD | Enable LCD endpoint. | bool | false |
| enableEvmRPC | Enable EVM RPC endpoint. | bool | false |
| enableEvmRpcWS | Enable EVM RPC Websocket endpoint. | bool | false |
| host | Host in which endpoints will be exposed. Endpoints are exposed on corresponding subdomain of this host. An example host `nodes.example.com` will have endpoints exposed at `rpc.nodes.example.com`, `grpc.nodes.example.com` and `lcd.nodes.example.com`. | string | true |
| annotations | Annotations to be set on ingress resource. | map[string]string | false |
| disableTLS | Whether to disable TLS on ingress resource. | bool | false |
| tlsSecretName | Name of the secret containing TLS certificate. | *string | false |
| grpcAnnotations | GrpcAnnotations to be set on grpc ingress resource. Defaults to nginx annotation `nginx.ingress.kubernetes.io/backend-protocol: GRPC` if nginx ingress class is used. | map[string]string | false |
| ingressClass | IngressClass specifies the ingress class to be used on ingresses | *string | false |
| useInternalServices | UseInternalServices configures Ingress to route traffic directly to the node services, bypassing Cosmoguard and any readiness checks. This is only recommended for debugging or for private/internal traffic (e.g., when accessing the cluster over a VPN). | *bool | false |
| servicesOnly | ServicesOnly indicates that only global services should be created. No ingress resources will be created. Useful for usage with custom controllers that have their own CRDs. | *bool | false |

[Back to Custom Resources](#custom-resources)

#### IndividualIngressConfig

IndividualIngressConfig provides host configuration for individual node ingresses.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| host |  | string | true |

[Back to Custom Resources](#custom-resources)

#### IngressConfig

IngressConfig specifies configurations for ingress to expose API endpoints.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| enableRPC | Enable RPC endpoint. | bool | false |
| enableGRPC | Enable gRPC endpoint. | bool | false |
| enableLCD | Enable LCD endpoint. | bool | false |
| enableEvmRPC | Enable EVM RPC endpoint. | bool | false |
| enableEvmRpcWS | Enable EVM RPC Websocket endpoint. | bool | false |
| host | Host in which endpoints will be exposed. Endpoints are exposed on corresponding subdomain of this host. An example host `nodes.example.com` will have endpoints exposed at `rpc.nodes.example.com`, `grpc.nodes.example.com` and `lcd.nodes.example.com`. | string | true |
| annotations | Annotations to be appended to the ingress. | map[string]string | false |
| disableTLS | Whether to disable TLS on ingress resource. | bool | false |
| tlsSecretName | Name of the secret containing TLS certificate. | *string | false |
| grpcAnnotations | GrpcAnnotations to be set on grpc ingress resource. Defaults to nginx annotation `nginx.ingress.kubernetes.io/backend-protocol: GRPC` if nginx ingress class is used. | map[string]string | false |
| ingressClass | IngressClass specifies the ingress class to be used on ingresses | *string | false |
| useInternalServices | UseInternalServices configures Ingress to route traffic directly to the node services, bypassing Cosmoguard and any readiness checks. This is only recommended for debugging or for private/internal traffic (e.g., when accessing the cluster over a VPN). | *bool | false |

[Back to Custom Resources](#custom-resources)

#### NodeGroupSpec

NodeGroupSpec sets chainnode configurations for a group.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| name | Name of this group. | string | true |
| instances | Number of ChainNode instances to run on this group. | *int | false |
| config | Specific configurations for these nodes. | *[Config](#config) | false |
| persistence | Configures PVC for persisting data. Automated data snapshots can also be configured in this section. | *[Persistence](#persistence) | false |
| peers | Additional persistent peers that should be added to these nodes. | [][Peer](#peer) | false |
| expose | Allows exposing P2P traffic to public. | *[ExposeConfig](#exposeconfig) | false |
| ingress | Ingress defines configuration for exposing API endpoints through a single shared Ingress, which routes traffic to the group Service backing all nodes in this set. This results in load-balanced access across all nodes (e.g., round-robin). See IngressConfig for detailed endpoint and TLS settings. | *[IngressConfig](#ingressconfig) | false |
| individualIngresses | IndividualIngresses defines configuration for exposing API endpoints through separate Ingress resources per node in the set. Each Ingress routes traffic directly to its corresponding node's Service (i.e., no load balancing across nodes).\n\nThe same IngressConfig is reused for all nodes, but the `host` field will be prefixed with the node index to generate unique subdomains. For example, if `host = \"fullnodes.cosmopilot.local\"`, then node ingress domains will be:\n  - 0.fullnodes.cosmopilot.local\n  - 1.fullnodes.cosmopilot.local\n  - etc.\n\nThis mode allows targeting specific nodes individually. | *[IngressConfig](#ingressconfig) | false |
| resources | Compute Resources required by the app container. | corev1.ResourceRequirements | false |
| nodeSelector | Selector which must be true for the pod to fit on a node. Selector which must match a node's labels for the pod to be scheduled on that node. | map[string]string | false |
| affinity | If specified, the pod's scheduling constraints. | *corev1.Affinity | false |
| stateSyncRestore | Configures these nodes to find state-sync snapshots on the network and restore from it. This is disabled by default. | *bool | false |
| inheritValidatorGasPrice | Whether these nodes should inherit gas price from validator (if there is not configured on this ChainNodeSet) Defaults to `true`. | *bool | false |
| ignoreGroupOnDisruptionChecks | Whether ChainNodeSet group label should be ignored on pod disruption checks. This is useful to ensure no downtime globally or per global ingress, instead of just per group. Defaults to `false`. | *bool | false |
| vpa | Vertical Pod Autoscaling configuration for this node. | *[VerticalAutoscalingConfig](#verticalautoscalingconfig) | false |
| pdb | Pod Disruption Budget configuration for this group. | *[PdbConfig](#pdbconfig) | false |
| snapshotNodeIndex | Index of the node in the group to take volume snapshots from (if enabled). Defaults to `0`. | *int | false |
| overrideVersion | OverrideVersion will force this group to use the specified version. NOTE: when this is set, cosmopilot will not upgrade the nodes, nor will set the version based on upgrade history. For unsetting this, you will have to do it here and individually per ChainNode | *string | false |

[Back to Custom Resources](#custom-resources)

#### NodeSetValidatorConfig

NodeSetValidatorConfig contains validator configurations.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| privateKeySecret | Secret containing the private key to be used by this validator. Defaults to `<chainnode>-priv-key`. Will be created if it does not exist. | *string | false |
| info | Contains information details about the validator. | *[ValidatorInfo](#validatorinfo) | false |
| init | Specifies configs and initialization commands for creating a new genesis. | *[GenesisInitConfig](#genesisinitconfig) | false |
| config | Allows setting specific configurations for the validator. | *[Config](#config) | false |
| persistence | Configures PVC for persisting data. Automated data snapshots can also be configured in this section. | *[Persistence](#persistence) | false |
| resources | Compute Resources required by the app container. | corev1.ResourceRequirements | false |
| nodeSelector | Selector which must be true for the pod to fit on a node. Selector which must match a node's labels for the pod to be scheduled on that node. | map[string]string | false |
| affinity | If specified, the pod's scheduling constraints. | *corev1.Affinity | false |
| tmKMS | TmKMS configuration for signing commits for this validator. When configured, .spec.validator.privateKeySecret will not be mounted on the validator node. | *[TmKMS](#tmkms) | false |
| stateSyncRestore | Configures this node to find a state-sync snapshot on the network and restore from it. This is disabled by default. | *bool | false |
| createValidator | Indicates cosmopilot should run create-validator tx to make this node a validator. | *[CreateValidatorConfig](#createvalidatorconfig) | false |
| vpa | Vertical Pod Autoscaling configuration for this node. | *[VerticalAutoscalingConfig](#verticalautoscalingconfig) | false |
| pdb | Pod Disruption Budget configuration for the validator pod. This is mainly useful in testnets where multiple validators might run in the same namespace. In production mainnet environments, where typically only one validator runs per namespace, this is rarely needed. | *[PdbConfig](#pdbconfig) | false |
| overrideVersion | OverrideVersion will force validator to use the specified version. NOTE: when this is set, cosmopilot will not upgrade the node, nor will set the version based on upgrade history. For unsetting this, you will have to do it here and on the ChainNode itself. | *string | false |
| ingress | Indicates if an ingress should be created to access API endpoints of validator node and configures it. | *[IngressConfig](#ingressconfig) | false |

[Back to Custom Resources](#custom-resources)

#### PdbConfig

PdbConfig configures the Pod Disruption Budget for a pod.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| enabled | Enabled specifies whether to deploy a Pod Disruption Budget. | bool | true |
| minAvailable | MinAvailable indicates minAvailable field set in PDB. Defaults to the number of instances in the group minus 1, i.e. it allows only a single disruption. | *int | false |

[Back to Custom Resources](#custom-resources)

#### SeedStatus

SeedStatus contains status information about a cosmoseed node.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| name |  | string | true |
| id |  | string | true |
| publicAddress |  | string | false |

[Back to Custom Resources](#custom-resources)

#### AccountAssets

AccountAssets represents the assets associated with an account.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| address | Address of the account. | string | true |
| assets | Assets assigned to this account. | []string | true |

[Back to Custom Resources](#custom-resources)

#### AppSpec

AppSpec specifies the source image, version and binary name of the app to run. Also allows specifying upgrades for the app and enabling automatic check of upgrade proposals on chain.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| image | Container image to be used. | string | true |
| version | Image tag to be used. Once there are completed or skipped upgrades this will be ignored. For a new node that will be state-synced, this will be the version used during state-sync. Only after that, the cosmopilot will switch to the version of last upgrade. Defaults to `latest`. | *string | false |
| imagePullPolicy | Indicates the desired pull policy when creating nodes. Defaults to `Always` if `version` is `latest` and `IfNotPresent` otherwise. | corev1.PullPolicy | false |
| app | Binary name of the application to be run. | string | true |
| sdkVersion | SdkVersion specifies the version of cosmos-sdk used by this app. Valid options are: - \"v0.47\" (default) - \"v0.45\" | *SdkVersion | false |
| checkGovUpgrades | Whether cosmopilot should query gov proposals to find and schedule upgrades. Defaults to `true`. | *bool | false |
| upgrades | List of upgrades to schedule for this node. | [][UpgradeSpec](#upgradespec) | false |

[Back to Custom Resources](#custom-resources)

#### ChainNodeAssets

ChainNodeAssets represents the assets associated with an account from another ChainNode.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| chainNode | Name of the ChainNode. | string | true |
| assets | Assets assigned to this account. | []string | true |

[Back to Custom Resources](#custom-resources)

#### Config

Config allows setting specific configurations for a node, including overriding configs in app.toml and config.toml.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| override | Allows overriding configs on `.toml` configuration files. | *map[string]runtime.RawExtension | false |
| sidecars | Allows configuring additional containers to run alongside the node. | [][SidecarSpec](#sidecarspec) | false |
| imagePullSecrets | Optional list of references to secrets in the same namespace to use for pulling any of the images used by this node. | []corev1.LocalObjectReference | false |
| blockThreshold | The time to wait for a block before considering node unhealthy. Defaults to `15s`. | *string | false |
| reconcilePeriod | Period at which a reconcile loop will happen for this ChainNode. Defaults to `15s`. | *string | false |
| stateSync | Allows configuring this node to perform state-sync snapshots. | *[StateSyncConfig](#statesyncconfig) | false |
| seedMode | Configures this node to run on seed mode. Defaults to `false`. | *bool | false |
| env | List of environment variables to set in the app container. | []corev1.EnvVar | false |
| safeToEvict | SafeToEvict sets cluster-autoscaler.kubernetes.io/safe-to-evict annotation to the given value. It allows/disallows cluster-autoscaler to evict this node's pod. | *bool | false |
| cosmoGuard | Deploys CosmoGuard to protect API endpoints of the node. | *[CosmoGuardConfig](#cosmoguardconfig) | false |
| nodeUtilsLogLevel | Log level for node-utils container. Defaults to `info`. | *string | false |
| startupTime | The time after which a node will be restarted if it does not start properly. Defaults to `1h`. | *string | false |
| ignoreSyncing | Marks the node as ready even when it is catching up. This is useful when a chain is halted, but you still need the node to be ready for querying existing data. Defaults to `false`. | *bool | false |
| nodeUtilsResources | Compute Resources for node-utils container. | *corev1.ResourceRequirements | false |
| persistAddressBook | Whether to persist address book file in data directory. Defaults to `false`. | *bool | false |
| terminationGracePeriodSeconds | Optional duration in seconds the pod needs to terminate gracefully. | *int64 | false |
| evmEnabled | Whether EVM is enabled on this node. Will add evm-rpc port to services. Defaults to `false`. | *bool | false |
| runFlags | List of flags to be appended to app container when starting the node. | []string | false |
| volumes | Additional volumes to be created and mounted on this node. | [][VolumeSpec](#volumespec) | false |
| dashedConfigToml | Whether field naming in config.toml should use dashes instead of underscores. Defaults to `false`. | *bool | false |
| haltHeight | The block height at which the node should stop. Cosmopilot will not attempt to restart the node beyond this height. | *int64 | false |

[Back to Custom Resources](#custom-resources)

#### CosmoGuardConfig

CosmoGuardConfig allows configuring CosmoGuard rules.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| enable | Whether to enable CosmoGuard on this node. | bool | true |
| config | ConfigMap containing the CosmoGuard configuration for this node. | *corev1.ConfigMapKeySelector | true |
| restartPodOnFailure | Whether the node's pod should be restarted when CosmoGuard fails. | *bool | false |
| resources | Compute Resources for CosmoGuard container. | *corev1.ResourceRequirements | false |

[Back to Custom Resources](#custom-resources)

#### CreateValidatorConfig

CreateValidatorConfig holds configuration for cosmopilot to submit a create-validator transaction.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| accountMnemonicSecret | Name of the secret containing the mnemonic of the account to be used by this validator. Defaults to `<chainnode>-account`. Will be created if it does not exist. | *string | false |
| accountHDPath | HD path of accounts. Defaults to `m/44'/118'/0'/0/0`. | *string | false |
| accountPrefix | Prefix for accounts. Defaults to `nibi`. | *string | false |
| valPrefix | Prefix for validator operator accounts. Defaults to `nibivaloper`. | *string | false |
| commissionMaxChangeRate | Maximum commission change rate percentage (per day). Defaults to `0.1`. | *string | false |
| commissionMaxRate | Maximum commission rate percentage. Defaults to `0.1`. | *string | false |
| commissionRate | Initial commission rate percentage. Defaults to `0.1`. | *string | false |
| minSelfDelegation | Minimum self delegation required on the validator. Defaults to `1`. | *string | false |
| stakeAmount | Amount to be staked by this validator. | string | true |
| gasPrices | Gas prices in decimal format to determine the transaction fee. | string | true |

[Back to Custom Resources](#custom-resources)

#### ExportTarballConfig

ExportTarballConfig holds config options for tarball upload.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| suffix | Suffix to add to archive name. The name of the tarball will be `<chain-id>-<timestamp>-<suffix>`. | *string | false |
| deleteOnExpire | Whether to delete the tarball when the snapshot expires. Default is `false`. | *bool | false |
| gcs | Configuration to upload tarballs to a GCS bucket. | *[GcsExportConfig](#gcsexportconfig) | false |

[Back to Custom Resources](#custom-resources)

#### ExposeConfig

ExposeConfig allows configuring how P2P endpoint is exposed to public.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| p2p | Whether to expose p2p endpoint for this node. Defaults to `false`. | *bool | false |
| p2pServiceType | P2pServiceType indicates how P2P port will be exposed. Valid values are: - `LoadBalancer` - `NodePort` (default) | *corev1.ServiceType | false |
| annotations | Annotations to be appended to the p2p service. | map[string]string | false |

[Back to Custom Resources](#custom-resources)

#### FromNodeRPCConfig

FromNodeRPCConfig holds configuration to retrieve genesis from an existing node using RPC endpoint.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| secure | Defines protocol to use. Defaults to `false`. | bool | false |
| hostname | Hostname or IP address of the RPC server. | string | true |
| port | TCP port used for RPC queries on the RPC server. Defaults to `26657`. | *int | false |

[Back to Custom Resources](#custom-resources)

#### GcsExportConfig

GcsExportConfig holds required settings to upload to GCS.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| bucket | Name of the bucket to upload tarballs to. | string | true |
| credentialsSecret | Secret with the JSON credentials to upload to bucket. | *corev1.SecretKeySelector | true |
| sizeLimit | Size limit at which the file will be split into multiple parts. Defaults to `5TB`. | *string | false |
| partSize | Size of each part when size-limit is crossed. Defaults to `500GB`. | *string | false |
| chunkSize | Size of each chunk uploaded in parallel to GCS. Defaults to `250MB`. | *string | false |
| bufferSize | Size of the buffer when streaming data to GCS. Defaults to `32MB`. | *string | false |
| concurrentJobs | Number of concurrent upload or delete jobs. Defaults to `10`. | *int | false |

[Back to Custom Resources](#custom-resources)

#### GenesisConfig

GenesisConfig specifies how genesis will be retrieved.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| url | URL to download the genesis from. | *string | false |
| fromNodeRPC | Get the genesis from an existing node using its RPC endpoint. | *[FromNodeRPCConfig](#fromnoderpcconfig) | false |
| genesisSHA | SHA256 to validate the genesis. | *string | false |
| configMap | ConfigMap specifies a configmap to load the genesis from. It can also be used to specify the name of the configmap to store the genesis when retrieving genesis using other methods. | *string | false |
| useDataVolume | UseDataVolume indicates that cosmopilot should save the genesis in the same volume as node data instead of a ConfigMap. This is useful for genesis whose size is bigger than ConfigMap limit of 1MiB. Ignored when genesis source is a ConfigMap. Defaults to `false`. | *bool | false |
| chainID | The chain-id of the network. This is only used when useDataVolume is true. If not set, cosmopilot will download the genesis and extract chain-id from it. If set, cosmopilot will not download it and use a container to download the genesis directly into the volume instead. This is useful for huge genesis that might kill cosmopilot container for using too much memory. | *string | false |

[Back to Custom Resources](#custom-resources)

#### GenesisInitConfig

GenesisInitConfig specifies configs and initialization commands for creating a new genesis.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| chainID | ChainID of the chain to initialize. | string | true |
| accountMnemonicSecret | Name of the secret containing the mnemonic of the account to be used by this validator. Defaults to `<chainnode>-account`. Will be created if it does not exist. | *string | false |
| accountHDPath | HD path of accounts. Defaults to `m/44'/118'/0'/0/0`. | *string | false |
| accountPrefix | Prefix for accounts. Defaults to `nibi`. | *string | false |
| valPrefix | Prefix for validator operator accounts. Defaults to `nibivaloper`. | *string | false |
| commissionMaxChangeRate | Maximum commission change rate percentage (per day). Defaults to `0.1`. | *string | false |
| commissionMaxRate | Maximum commission rate percentage. Defaults to `0.1`. | *string | false |
| commissionRate | Initial commission rate percentage. Defaults to `0.1`. | *string | false |
| minSelfDelegation | Minimum self delegation required on the validator. Defaults to `1`. NOTE: In most chains this is a required flag. However, in a few other chains (Cosmos Hub for example), this flag does not even exist anymore. In those cases, set it to an empty string and cosmopilot will skip it. | *string | false |
| assets | Assets is the list of tokens and their amounts to be assigned to this validators account. | []string | true |
| stakeAmount | Amount to be staked by this validator. | string | true |
| accounts | Accounts specify additional accounts and respective assets to be added to this chain. | [][AccountAssets](#accountassets) | false |
| chainNodeAccounts | List of ChainNodes whose accounts should be included in genesis. NOTE: Cosmopilot will wait for the ChainNodes to exist and have accounts before proceeding. | [][ChainNodeAssets](#chainnodeassets) | false |
| unbondingTime | Time required to totally unbond delegations. Defaults to `1814400s` (21 days). | *string | false |
| votingPeriod | Voting period for this chain. Defaults to `120h`. | *string | false |
| additionalInitCommands | Additional commands to run on genesis initialization. Note: App home is at `/home/app` and `/temp` is a temporary volume shared by all init containers. | [][InitCommand](#initcommand) | false |

[Back to Custom Resources](#custom-resources)

#### InitCommand

InitCommand represents an initialization command. It may be used for running additional commands on genesis or volume initialization.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| image | Image to be used to run this command. Defaults to app image. | *string | false |
| command | Command to be used. Defaults to image entrypoint. | []string | false |
| args | Args to be passed to this command. | []string | true |

[Back to Custom Resources](#custom-resources)

#### Peer

Peer represents a peer.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| id | Tendermint node ID for this node. | string | true |
| address | Hostname or IP address of this peer. | string | true |
| port | P2P port to be used. Defaults to `26656`. | *int | false |
| unconditional | Indicates this peer is unconditional. | *bool | false |
| private | Indicates this peer is private. | *bool | false |
| seed | Indicates this is a seed. | *bool | false |

[Back to Custom Resources](#custom-resources)

#### Persistence

Persistence configuration for a node.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| size | Size of the persistent volume for storing data. Can't be updated when autoResize is enabled. Defaults to `50Gi`. | *string | false |
| storageClass | Name of the storage class to use for the PVC. Uses the default class if not specified. to create persistent volumes. | *string | false |
| autoResize | Automatically resize PVC. Defaults to `true`. | *bool | false |
| autoResizeThreshold | Percentage of data usage at which an auto-resize event should occur. Defaults to `80`. | *int | false |
| autoResizeIncrement | Increment size on each auto-resize event. Defaults to `50Gi`. | *string | false |
| autoResizeMaxSize | Size at which auto-resize will stop incrementing PVC size. Defaults to `2Ti`. | *string | false |
| additionalInitCommands | Additional commands to run on data initialization. Useful for downloading and extracting snapshots. App home is at `/home/app` and data dir is at `/home/app/data`. There is also `/temp`, a temporary volume shared by all init containers. | [][InitCommand](#initcommand) | false |
| snapshots | Whether cosmopilot should create volume snapshots according to this config. | *[VolumeSnapshotsConfig](#volumesnapshotsconfig) | false |
| restoreFromSnapshot | Restore from the specified snapshot when creating the PVC for this node. | *[PvcSnapshot](#pvcsnapshot) | false |
| initTimeout | Time to wait for data initialization pod to be successful. Defaults to `5m`. | *string | false |

[Back to Custom Resources](#custom-resources)

#### PvcSnapshot

PvcSnapshot represents a snapshot to be used to restore a PVC.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| name | Name of the volume snapshot being referenced. | string | true |

[Back to Custom Resources](#custom-resources)

#### SidecarSpec

SidecarSpec allows configuring additional containers to run alongside the node.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| name | Name to be assigned to the container. | string | true |
| image | Container image to be used. Defaults to app image being used by ChainNode. | *string | true |
| imagePullPolicy | Indicates the desired pull policy when creating nodes. Defaults to `Always` if `version` is `latest` and `IfNotPresent` otherwise. | corev1.PullPolicy | false |
| mountDataVolume | Where data volume will be mounted on this container. It is not mounted if not specified. | *string | false |
| mountConfig | Directory where config files from ConfigMap will be mounted on this container. They are not mounted if not specified. | *string | false |
| command | Command to be run by this container. Defaults to entrypoint defined in image. | []string | false |
| args | Args to be passed to this container. Defaults to cmd defined in image. | []string | false |
| env | Environment variables to be passed to this container. | []corev1.EnvVar | false |
| securityContext | Security options the container should be run with. | *corev1.SecurityContext | false |
| resources | Compute Resources for the sidecar container. | corev1.ResourceRequirements | false |
| restartPodOnFailure | Whether the pod of this node should be restarted when this sidecar container fails. Defaults to `false`. | *bool | false |
| runBeforeNode | When enabled, this container turns into an init container instead of a sidecar as it will have to finish before the node container starts. Defaults to `false`. | *bool | false |

[Back to Custom Resources](#custom-resources)

#### StateSyncConfig

StateSyncConfig holds configurations for enabling state-sync snapshots on a node.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| snapshotInterval | Block interval at which local state sync snapshots are taken (0 to disable). | int | true |
| snapshotKeepRecent | Number of recent snapshots to keep and serve (0 to keep all). Defaults to 2. | *int | false |

[Back to Custom Resources](#custom-resources)

#### TmKMS

TmKMS allows configuring tmkms for signing for this validator node instead of using plaintext private key file.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| provider | Signing provider to be used by tmkms. Currently only `vault` is supported. | [TmKmsProvider](#tmkmsprovider) | true |
| keyFormat | Format and type of key for chain. Defaults to `{\"type\": \"bech32\", \"account_key_prefix\": \"nibipub\", \"consensus_key_prefix\": \"nibivalconspub\"}`. | *[TmKmsKeyFormat](#tmkmskeyformat) | false |
| validatorProtocol | Tendermint's protocol version to be used. Valid options are: - `v0.34` (default) - `v0.33` - `legacy` | *tmkms.ProtocolVersion | false |
| persistState | Whether to persist \"priv_validator_state.json\" file on a PVC. Defaults to `true`. | *bool | false |
| resources | Compute Resources for tmkms container. | *corev1.ResourceRequirements | false |

[Back to Custom Resources](#custom-resources)

#### TmKmsHashicorpProvider

TmKmsHashicorpProvider holds `hashicorp` provider specific configurations.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| address | Full address of the Vault cluster. | string | true |
| key | Key to be used by this validator. | string | true |
| certificateSecret | Secret containing the CA certificate of the Vault cluster. | *corev1.SecretKeySelector | false |
| tokenSecret | Secret containing the token to be used. | *corev1.SecretKeySelector | true |
| uploadGenerated | UploadGenerated indicates if the controller should upload the generated private key to vault. Defaults to `false`. Will be set to `true` if this validator is initializing a new genesis. This should not be used in production. | bool | false |
| autoRenewToken | Whether to automatically renew vault token. Defaults to `false`. | bool | false |
| skipCertificateVerify | Whether to skip certificate verification. Defaults to `false`. | bool | false |

[Back to Custom Resources](#custom-resources)

#### TmKmsKeyFormat

TmKmsKeyFormat represents key format for tmKMS.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| type | Type specifies the key format type. | string | true |
| account_key_prefix | AccountKeyPrefix is the prefix used for account keys. | string | true |
| consensus_key_prefix | ConsensusKeyPrefix is the prefix used for consensus keys. | string | true |

[Back to Custom Resources](#custom-resources)

#### TmKmsProvider

TmKmsProvider allows configuring providers for tmKMS. Note that only one should be configured.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| hashicorp | Hashicorp provider. | *[TmKmsHashicorpProvider](#tmkmshashicorpprovider) | false |

[Back to Custom Resources](#custom-resources)

#### Upgrade

Upgrade represents an upgrade processed by cosmopilot and added to status.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| height | Height at which the upgrade should occur. | int64 | true |
| image | Container image replacement to be used in the upgrade. | string | true |
| status | Upgrade status. | UpgradePhase | true |
| source | Where cosmopilot got this upgrade from. | UpgradeSource | true |

[Back to Custom Resources](#custom-resources)

#### UpgradeSpec

UpgradeSpec represents a manual upgrade.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| height | Height at which the upgrade should occur. | int64 | true |
| image | Container image replacement to be used in the upgrade. | string | true |
| forceOnChain | Whether to force this upgrade to be processed as a gov planned upgrade. Defaults to `false`. | *bool | false |

[Back to Custom Resources](#custom-resources)

#### ValidatorInfo

ValidatorInfo contains information about this validator.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| moniker | Moniker to be used by this validator. Defaults to the ChainNode name. | *string | false |
| details | Details of this validator. | *string | false |
| website | Website of the validator. | *string | false |
| identity | Identity signature of this validator. | *string | false |

[Back to Custom Resources](#custom-resources)

#### VerticalAutoscalingConfig

VerticalAutoscalingConfig defines rules and thresholds for vertical autoscaling of a pod.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| enabled | Enables vertical autoscaling for the pod. | bool | true |
| cpu | CPU resource autoscaling configuration. | *[VerticalAutoscalingMetricConfig](#verticalautoscalingmetricconfig) | false |
| memory | Memory resource autoscaling configuration. | *[VerticalAutoscalingMetricConfig](#verticalautoscalingmetricconfig) | false |

[Back to Custom Resources](#custom-resources)

#### VerticalAutoscalingMetricConfig

VerticalAutoscalingMetricConfig defines autoscaling behavior for a specific resource type (CPU or memory).

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| source | Source determines whether to base autoscaling decisions on requests, limits, or effective limit. Valid values are: `effective-limit` (default) (use limits if set; otherwise fallback to requests) `requests` (use the pod’s requested resource value) `limits` (use the pod’s resource limit value) | *LimitSource | false |
| min | Minimum resource value allowed during scaling (e.g. \"100m\" or \"128Mi\"). | resource.Quantity | true |
| max | Maximum resource value allowed during scaling (e.g. \"8000m\" or \"2Gi\"). | resource.Quantity | true |
| rules | Rules define when and how scaling should occur based on sustained usage levels. | []*[VerticalAutoscalingRule](#verticalautoscalingrule) | true |
| cooldown | Cooldown is the minimum duration to wait between consecutive scaling actions. Defaults to \"5m\". | *string | false |
| limitStrategy | LimitStrategy controls how resource limits should be updated after autoscaling. Valid values are: `retain` (default) (keep original limits) `equal` (match request value) `max` (use configured VPA Max) `percentage` (request × percentage) `unset` (remove the limits field entirely) | *LimitUpdateStrategy | false |
| limitPercentage | LimitPercentage defines the percentage multiplier to apply when using \"percentage\" LimitStrategy. For example, 150 means limit = request * 1.5. Only used when LimitStrategy = \"percentage\". Defaults to `150` when not set. | *int | false |

[Back to Custom Resources](#custom-resources)

#### VerticalAutoscalingRule

VerticalAutoscalingRule defines a single rule for when to trigger a scaling adjustment.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| direction | Direction of scaling: \"up\" or \"down\". | ScalingDirection | true |
| usagePercent | UsagePercent is the resource usage percentage (0–100) that must be met. Usage is compared against the selected Source value. | int | true |
| duration | Duration is the length of time the usage must remain above/below the threshold before scaling. Defaults to \"5m\". | *string | false |
| stepPercent | StepPercent defines how much to adjust the resource by, as a percentage of the current value. For example, 50 = scale by 50% of current value. | int | true |

[Back to Custom Resources](#custom-resources)

#### VolumeSnapshotsConfig

VolumeSnapshotsConfig holds the configuration of snapshotting feature.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| frequency | How often a snapshot should be created. | string | true |
| retention | How long a snapshot should be retained. Default is indefinite retention. | *string | false |
| snapshotClass | Name of the volume snapshot class to be used. Uses the default class if not specified. | *string | false |
| stopNode | Whether the node should be stopped while the snapshot is taken. Defaults to `false`. | *bool | false |
| exportTarball | Whether to create a tarball of data directory in each snapshot and upload it to external storage. | *[ExportTarballConfig](#exporttarballconfig) | false |
| verify | Whether cosmopilot should verify the snapshot for corruption after it is ready. Defaults to `false`. | *bool | false |
| disableWhileSyncing | Whether to disable snapshots while the node is syncing. Defaults to `true`. | *bool | false |
| resources | Compute resources for the integrity-check job pod (applied only when `verify` is true). | corev1.ResourceRequirements | false |

[Back to Custom Resources](#custom-resources)

#### VolumeSpec

VolumeSpec describes an additional volume to mount on a node.

| Field | Description | Scheme | Required |
| ----- | ----------- | ------ | -------- |
| name | The name of the volume. | string | true |
| size | Size of the volume. | string | true |
| path | Path specifies where this volume should be mounted. | string | true |
| storageClass | Name of the storage class to use for this volume. Uses the default class if not specified. | *string | false |
| deleteWithNode | Whether this volume should be deleted when node is deleted. Defaults to `false`. | *bool | false |

[Back to Custom Resources](#custom-resources)
