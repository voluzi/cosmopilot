package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// TestValidatorConfigGetAccountSecretName verifies account-mnemonic secret resolution for a
// ChainNode validator. A create-validator node must use its configured (funded) operator account
// secret instead of the generated <chainnode>-account default; init takes precedence when both are
// set (they are mutually exclusive in practice).
func TestValidatorConfigGetAccountSecretName(t *testing.T) {
	node := &ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "mynode"}}

	tests := []struct {
		name string
		val  *ValidatorConfig
		want string
	}{
		{
			name: "no account configured falls back to the default",
			val:  &ValidatorConfig{},
			want: "mynode-account",
		},
		{
			name: "createValidator account secret is honored",
			val:  &ValidatorConfig{CreateValidator: &CreateValidatorConfig{AccountMnemonicSecret: ptr.To("funded-operator")}},
			want: "funded-operator",
		},
		{
			name: "createValidator without an account secret falls back to the default",
			val:  &ValidatorConfig{CreateValidator: &CreateValidatorConfig{}},
			want: "mynode-account",
		},
		{
			name: "init account secret is honored",
			val:  &ValidatorConfig{Init: &GenesisInitConfig{AccountMnemonicSecret: ptr.To("init-account")}},
			want: "init-account",
		},
		{
			name: "init account secret takes precedence over createValidator",
			val: &ValidatorConfig{
				Init:            &GenesisInitConfig{AccountMnemonicSecret: ptr.To("init-account")},
				CreateValidator: &CreateValidatorConfig{AccountMnemonicSecret: ptr.To("funded-operator")},
			},
			want: "init-account",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.val.GetAccountSecretName(node))
		})
	}
}
