package chainnodeset

import (
	"context"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
)

func (r *Reconciler) refuseLegacyTmKMS(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	guard := &r.legacySignerGuard
	if r.opts != nil {
		guard = &r.opts.LegacySignerGuard
	}
	return guard.RefuseLegacyTmKMSChildren(ctx, r.APIReader, r.recorder, nodeSet)
}
