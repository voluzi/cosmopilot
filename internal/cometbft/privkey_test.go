package cometbft

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetPubKey(t *testing.T) {
	tests := []struct {
		provided string
		expected string
	}{
		{
			provided: "{\"address\":\"0E85BC4610C7710A01CA6CC98E2CC5CFE9935690\",\"pub_key\":{\"type\":\"tendermint/PubKeyEd25519\",\"value\":\"WSNiGcovSATN09MKkaqFDOQgypn1FPDhVwfYIPFVp34=\"},\"priv_key\":{\"type\":\"tendermint/PrivKeyEd25519\",\"value\":\"24Jswwf3dxtD5r6byv8agLgjCuvoz2I7UXCsJmrjLp1ZI2IZyi9IBM3T0wqRqoUM5CDKmfUU8OFXB9gg8VWnfg==\"}}",
			expected: "{\"@type\":\"/cosmos.crypto.ed25519.PubKey\",\"key\":\"WSNiGcovSATN09MKkaqFDOQgypn1FPDhVwfYIPFVp34=\"}",
		},
	}

	for _, test := range tests {
		pubKey, err := GetPubKey([]byte(test.provided))
		assert.NoError(t, err)
		assert.Equal(t, test.expected, pubKey)
	}
}

func TestLoadPrivKeyRejectsMalformedTypedKey(t *testing.T) {
	key := []byte(`{
		"address":"0000000000000000000000000000000000000000",
		"pub_key":{"type":"tendermint/PubKeyEd25519","value":"eA=="},
		"priv_key":{"type":"tendermint/PrivKeyEd25519","value":"eA=="}
	}`)

	_, err := LoadPrivKey(key)
	assert.Error(t, err)
}

func TestLoadPrivKeyRejectsInconsistentKey(t *testing.T) {
	firstJSON, err := GeneratePrivKey()
	assert.NoError(t, err)
	first, err := LoadPrivKey(firstJSON)
	assert.NoError(t, err)

	secondJSON, err := GeneratePrivKey()
	assert.NoError(t, err)
	second, err := LoadPrivKey(secondJSON)
	assert.NoError(t, err)

	first.PrivKey = second.PrivKey
	inconsistent, err := json.Marshal(first)
	assert.NoError(t, err)
	_, err = LoadPrivKey(inconsistent)
	assert.Error(t, err)
}
