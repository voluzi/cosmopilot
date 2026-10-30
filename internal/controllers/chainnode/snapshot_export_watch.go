package chainnode

import (
	"context"
	"fmt"
	"strings"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/voluzi/cosmopilot/v5/internal/exportprotocol"
)

func nativeExportEvent(o client.Object) bool {
	return o.GetLabels()[exportprotocol.OwnerNameLabel] != "" || o.GetAnnotations()[exportprotocol.HoldAnnotation] != ""
}
func (r *Reconciler) nativeExportRequests(ctx context.Context, o client.Object) []reconcile.Request {
	requests := []reconcile.Request{}
	if name := o.GetLabels()[exportprotocol.OwnerNameLabel]; name != "" && o.GetLabels()[exportprotocol.OwnerUIDLabel] != "" {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: name}})
	}
	if s, ok := o.(*snapshotv1.VolumeSnapshot); ok {
		holders, err := nativeHolders(s)
		if err != nil {
			log.FromContext(ctx).Error(fmt.Errorf("%.256s", err.Error()), "decode native snapshot holders", "namespace", s.Namespace, "name", s.Name)
			return requests
		}
		for _, holder := range holders {
			parts := strings.Split(holder, "/")
			if len(parts) == 5 && parts[0] == s.Namespace && parts[2] != "" {
				requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: parts[0], Name: parts[1]}})
			}
		}
	}
	return requests
}
