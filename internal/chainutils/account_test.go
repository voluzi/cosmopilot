package chainutils

import (
	"testing"

	"github.com/stretchr/testify/assert"

	appsv1 "github.com/NibiruChain/cosmopilot/api/v1"
)

const testMnemonic = "upset promote follow flag you way eagle plunge scorpion oil version afraid churn fog tiger almost noise define license pistol post raise report time"

func TestAccountFromMnemonic(t *testing.T) {
	tests := []struct {
		name     string
		provided string
		expected *Account
		wantErr  bool
	}{
		{
			name:     "valid mnemonic",
			provided: testMnemonic,
			expected: &Account{
				Mnemonic:         testMnemonic,
				Address:          "nibi1ll3njapxnyqqvfz65puwvmmya23a0xcqhfkkat",
				ValidatorAddress: "nibivaloper1ll3njapxnyqqvfz65puwvmmya23a0xcq7jcdfk",
			},
		},
		{
			name:     "invalid mnemonic",
			provided: "invalid mnemonic",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := AccountFromMnemonic(tt.provided, appsv1.DefaultAccountPrefix, appsv1.DefaultValPrefix, appsv1.DefaultHDPath)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tt.expected.Mnemonic, result.Mnemonic)
			assert.Equal(t, tt.expected.ValidatorAddress, result.ValidatorAddress)
			assert.Equal(t, tt.expected.Address, result.Address)
		})
	}
}

func TestAccountAddressFromValidatorAddress(t *testing.T) {
	tests := []struct {
		name     string
		provided string
		expected string
	}{
		{
			name:     "validator to account",
			provided: "nibivaloper1efeydq3s4wgrv5yslxcevsstwtrkmkel5zkqgx",
			expected: "nibi1efeydq3s4wgrv5yslxcevsstwtrkmkelaecmum",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := AccountAddressFromValidatorAddress(tt.provided, appsv1.DefaultValPrefix, appsv1.DefaultAccountPrefix)
			assert.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}
