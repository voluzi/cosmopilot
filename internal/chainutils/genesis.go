package chainutils

import (
	"encoding/json"
	"fmt"
)

func ExtractChainIdFromGenesis(genesis string) (string, error) {
	var genesisJson map[string]interface{}
	if err := json.Unmarshal([]byte(genesis), &genesisJson); err != nil {
		return "", err
	}
	if chainId, ok := genesisJson["chain_id"]; ok {
		return chainId.(string), nil
	}
	return "", fmt.Errorf("could not extract chain id from genesis")
}
