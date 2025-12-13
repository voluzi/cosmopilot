package sdkcmd

import (
	appsv1 "github.com/voluzi/cosmopilot/api/v1"
)

func init() {
	RegisterSDK(appsv1.V0_50, func(globalOptions ...Option) SDK {
		return newV0_50(globalOptions...)
	})
}

func newV0_50(globalOptions ...Option) *v0_50 {
	return &v0_50{v0_47: *newV0_47(globalOptions...)}
}

type v0_50 struct {
	v0_47
}
