package chainnode

import (
	"testing"

	appsv1 "github.com/voluzi/cosmopilot/v4/api/v1"
)

func TestSignerTargetConfigListensOnPrivValPort(t *testing.T) {
	for _, dashed := range []bool{false, true} {
		for _, validator := range []bool{false, true} {
			node := &appsv1.ChainNode{Spec: appsv1.ChainNodeSpec{RemoteSignerTarget: true, Config: &appsv1.Config{}}}
			if validator {
				node.Spec.Validator = &appsv1.ValidatorConfig{}
			}
			formatter := GetKeyFormatter(node)
			formatter.UseDashes = dashed
			if got := formatter.GetBaseConfigToml()[formatter.PrivValidatorLaddr()]; got != "tcp://0.0.0.0:26659" {
				t.Fatalf("privval listener = %v", got)
			}
		}
	}
}
