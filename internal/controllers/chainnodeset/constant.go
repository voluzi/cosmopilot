package chainnodeset

import "time"

const (
	ChainNodeWaitTimeout = 3 * time.Minute
	ChainNodeKind        = "ChainNode"

	scopeGlobal = "global"
	scopeGroup  = "group"

	ingressClassNameNginx = "nginx"

	validatorGroupName = "validator"

	cosmoseedMountPoint     = "/cosmoseed"
	cosmoseedConfigFileName = "config.yaml"
	cosmoseedAddrBookDir    = "data"
	cosmoseedHttpPortName   = "http"
	cosmoseedHttpPort       = 8080
	cosmoseedP2pPort        = 26656

	timeoutWaitServiceIP = 5 * time.Minute
)

var (
	nginxGrpcAnnotations = map[string]string{
		"nginx.ingress.kubernetes.io/backend-protocol": "GRPC",
	}
)
