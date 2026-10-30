package chainnode

import (
	"testing"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/voluzi/cosmopilot/v5/internal/exportprotocol"
)

func TestNativeExportEventsRouteOnlyAttributedOwners(t *testing.T) {
	owner := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "owner"}}
	holder := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "holder"}}
	for _, tc := range []struct {
		name string
		obj  client.Object
		want []reconcile.Request
	}{
		{"label attribution", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Labels: map[string]string{exportprotocol.OwnerNameLabel: "owner", exportprotocol.OwnerUIDLabel: "uid"}}}, []reconcile.Request{owner}},
		{"name without UID", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Labels: map[string]string{exportprotocol.OwnerNameLabel: "owner"}}}, nil},
		{"UID without name", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Labels: map[string]string{exportprotocol.OwnerUIDLabel: "uid"}}}, nil},
		{"same namespace holder", &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Annotations: map[string]string{exportprotocol.HoldAnnotation: `{"attempt":"ns/holder/uid/1/attempt"}`}}}, []reconcile.Request{holder}},
		{"foreign namespace holder", &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Annotations: map[string]string{exportprotocol.HoldAnnotation: `{"attempt":"other/holder/uid/1/attempt"}`}}}, nil},
		{"malformed holder entries", &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Annotations: map[string]string{exportprotocol.HoldAnnotation: `{"bad":"ns/holder/uid","missing-uid":"ns/holder//1/missing-uid","valid":"ns/holder/uid/1/valid"}`}}}, []reconcile.Request{holder}},
		{"malformed JSON preserves labels", &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Labels: map[string]string{exportprotocol.OwnerNameLabel: "owner", exportprotocol.OwnerUIDLabel: "uid"}, Annotations: map[string]string{exportprotocol.HoldAnnotation: "{"}}}, []reconcile.Request{owner}},
		{"null holders", &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Annotations: map[string]string{exportprotocol.HoldAnnotation: "null"}}}, nil},
		{"holder annotation on another kind", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Annotations: map[string]string{exportprotocol.HoldAnnotation: `{"attempt":"ns/holder/uid/1/attempt"}`}}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []reconcile.Request
			if predicate.NewPredicateFuncs(nativeExportEvent).Generic(event.GenericEvent{Object: tc.obj}) {
				got = (&Reconciler{}).nativeExportRequests(t.Context(), tc.obj)
			}
			require.ElementsMatch(t, tc.want, got)
		})
	}
}
