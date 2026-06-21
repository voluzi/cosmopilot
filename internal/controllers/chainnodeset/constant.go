package chainnodeset

import (
	"time"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

const (
	ChainNodeWaitTimeout = 3 * time.Minute
	ChainNodeKind        = "ChainNode"

	scopeGlobal = "global"
	scopeGroup  = "group"

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
