# Exposing Endpoints

This page provides guidance on exposing endpoints for nodes managed by `Cosmopilot`. Exposing `API` endpoints relies on the creation of ingress resources. By default, these ingresses are created for the `nginx` ingress controller and secured using `cert-manager`. You can target a different ingress controller by setting the `ingressClass` field in your manifests.

The only exception is the `P2P` port, which can be exposed without any ingress controller.

## Exposing the P2P Port

The P2P port can be exposed by configuring the `.spec.expose` field on a `ChainNode` or the `.spec.nodes[].expose` field on a `ChainNodeSet`. By default, this feature is disabled.

```yaml
expose:
  p2p: true
  p2pServiceType: LoadBalancer # Optional. Default is `NodePort`.
  annotations: # Optional annotations to add to the P2P service.
    key1: value1
    key2: value2
```

### **Supported Service Types**
- **NodePort**: 
  - Exposes P2P traffic on a NodePort across all Kubernetes nodes.
  - This option is cost-effective as it uses existing Kubernetes node resources, but it requires Kubernetes nodes to have public access.
- **LoadBalancer**: 
  - On most cloud providers, this creates a dedicated Load Balancer mapped to the node.
  - Offers better performance but incurs higher costs due to the additional infrastructure.

## Exposing API Endpoints

API endpoints can be enabled through:

1. **ChainNode ingress**: Configure `.spec.ingress` on a standalone `ChainNode`.
2. **ChainNodeSet ingress**: Configure `.spec.ingresses` with one or multiple group names.

`spec.nodes[].ingress` is not a supported field. For separate per-node routes,
node groups support `nodes[].individualIngresses`.

### ChainNode Ingress Example

To expose API endpoints for a single `ChainNode`, use the following configuration under `spec`:

```yaml
ingress:
  host: node.example.com
  enableRPC: true # optional. Defaults to `false`.
  enableGRPC: true # optional. Defaults to `false`.
  enableLCD: true # optional. Defaults to `false`.
  ingressClass: traefik # optional. Defaults to `nginx`.
  disableTLS: false # optional. Defaults to `false`.
  tlsSecretName: node-tls # optional. Defaults to `<service-name>-tls`.
```

### Single-Group Ingress Example

To expose one group in a `ChainNodeSet`, select it under `spec.ingresses`:

```yaml
spec:
  ingresses:
    - name: fullnodes
      groups: [fullnodes]
      host: nodes.example.com
      enableRPC: true # optional. Defaults to `false`.
      enableGRPC: true # optional. Defaults to `false`.
      enableLCD: true # optional. Defaults to `false`.
      enableEvmRPC: false # optional. Defaults to `false`.
      enableEvmRpcWS: false # optional. Defaults to `false`.
      ingressClass: nginx # optional. Defaults to `nginx`.
      annotations:
        nginx.ingress.kubernetes.io/proxy-body-size: "50m"
      grpcAnnotations:
        nginx.ingress.kubernetes.io/backend-protocol: "GRPC"
        nginx.ingress.kubernetes.io/proxy-read-timeout: "300"
      disableTLS: false # optional. Defaults to `false`.
      tlsSecretName: example-tls-secret # optional. Defaults to `<service-name>-tls`.
```

`annotations` applies to the ordinary HTTP Ingress and is not copied to the
separate gRPC Ingress. Use `grpcAnnotations` for gRPC-specific settings. An explicit
`grpcAnnotations` map replaces the default map, so include `backend-protocol` when
using nginx.

### **Global Ingress Example**

To expose API endpoints for multiple groups of nodes, configure them under `.spec.ingresses`. Here’s an example:

```yaml
ingresses:
  - name: global-ingress
    groups:
      - fullnode
      - archive
    host: api.nodes.example.com
    enableRPC: true # optional. Defaults to `false`.
    enableGRPC: true # optional. Defaults to `false`.
    enableLCD: true # optional. Defaults to `false`.
    enableEvmRPC: true # optional. Defaults to `false`.
    enableEvmRpcWS: true # optional. Defaults to `false`.
    ingressClass: nginx # optional. Defaults to `nginx`.
    annotations: # optional annotations to ingress resource
      nginx.ingress.kubernetes.io/proxy-body-size: "100m"
    disableTLS: false # optional. Defaults to `false`.
    tlsSecretName: global-tls-secret # optional. Defaults to `<service-name>-tls`.
```

### Internal Services Only

If you only need internal `Service` resources (for example, when another controller manages ingress creation), set `servicesOnly` to `true` on a global ingress:

```yaml
ingresses:
  - name: internal-only
    groups: [fullnode]
    host: api.nodes.internal
    servicesOnly: true
```

:::info[NOTE]
Each `API` endpoint is exposed as a subdomain of the configured `host`. The following prefixes are defaults; override them with `subdomains` on either ingress configuration type.
- Tendermint RPC is available at `rpc.<host>`.
- Cosmos-SDK RPC is available at `lcd.<host>`.
- gRPC is available at `grpc.<host>`.
- EVM RPC is available at `evm-rpc.<host>`.
- EVM RPC Websocket is available at `evm-rpc-ws.<host>`.
:::

:::info[gRPC]
gRPC is served over HTTP/2 only, so it gets its own `<name>-grpc` Ingress backed by a gRPC-only Service of the same name. The ingress controller needs to know it must reach that backend over HTTP/2:
- `nginx` reads it from the Ingress, which gets `nginx.ingress.kubernetes.io/backend-protocol: GRPC` unless `grpcAnnotations` is set.
- Traefik reads it from the Service, which gets `traefik.ingress.kubernetes.io/service.serversscheme: h2c` when the ingress class contains `traefik`.
:::

### Recommended Approach

Use `.spec.ingresses` to expose one or multiple groups through a group-wide ingress.
