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

### P2P through a Gateway

Set `expose.p2p: true` and use `expose.gateway` in place of `expose.p2pServiceType`
to attach to a Gateway TCP listener. Cosmopilot manages `TCPRoute` resources as
`gateway.networking.k8s.io/v1`. This requires Gateway API CRDs **v1.6.0 or later**
and a Gateway implementation that supports TCPRoute.

#### Upgrading from v1alpha2

1. Before upgrading Cosmopilot, move to a bundle supported by your Gateway
   implementation that serves both TCPRoute versions, such as Gateway API
   v1.6.0 or v1.7.0 Experimental.
2. Upgrade Cosmopilot. Check that the Gateway accepts the existing routes and
   that peers can still connect to each listener from outside the cluster.
3. You can switch to a Standard bundle once every client has stopped using
   `v1alpha2`. Follow your Gateway implementation's and the
   [Gateway API bundle's upgrade guidance](https://github.com/kubernetes-sigs/gateway-api/releases/tag/v1.6.0)
   to update the CRDs in place. Keep the existing TCPRoutes and CRDs; deleting
   and recreating them is unnecessary.

On a cluster that serves only `v1alpha2`, existing TCPRoutes stay installed, but
Cosmopilot stops creating, updating, or cleaning them up until the CRDs serve
`v1`. A ChainNode in Gateway mode keeps its P2P Service if one already exists,
does not create a replacement, and clears its advertised public address.
Cosmoseed falls back to its P2P expose Service.

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
