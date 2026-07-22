package chainnodeset

import (
	"time"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

const (
	ChainNodeWaitTimeout = 3 * time.Minute
	ChainNodeKind        = "ChainNode"

	scopeGlobal     = "global"
	scopeGroup      = "group"
	scopeCosmoGuard = "cosmoguard"

	// cosmoGuardRouteLabelPrefix namespaces the per-route labels stamped on CosmoGuard pods so a
	// global ingress/gateway Service can select the guard pods of the groups it targets without
	// colliding with the bare route labels carried by node pods (which back the direct/bypass
	// Services).
	cosmoGuardRouteLabelPrefix = "route.cosmoguard.voluzi.com/"

	validatorGroupName = appsv1.ReservedValidatorGroupName

	cosmoseedMountPoint     = "/cosmoseed"
	cosmoseedConfigFileName = "config.yaml"
	cosmoseedAddrBookDir    = "data"
	cosmoseedHttpPortName   = "http"
	cosmoseedHttpPort       = 8080
	cosmoseedP2pPort        = 26656

	timeoutWaitServiceIP = 5 * time.Minute

	// mnemonicKey and privKeyFilename mirror the keys used by the ChainNode controller for the
	// account mnemonic and priv_validator_key.json secrets, so the secrets pre-created here for
	// genesis validators are reused as-is by the ChainNode controllers.
	mnemonicKey     = "mnemonic"
	privKeyFilename = "priv_validator_key.json"
)
