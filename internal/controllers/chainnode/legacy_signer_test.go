package chainnode

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
)

func TestReconcileRefusesLegacyTmKMS(t *testing.T) {
	for _, tc := range []struct {
		name               string
		pod, config, owned bool
		refused            bool
	}{
		{name: "pod", pod: true, refused: true},
		{name: "owned config without pod", config: true, owned: true, refused: true},
		{name: "unowned config", config: true},
		{name: "migrated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := appsv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "node-uid"}}
			owner := []metav1.OwnerReference{{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: node.Name, UID: node.UID, Controller: ptr.To(true)}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, OwnerReferences: owner}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}
			if tc.pod {
				pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "tmkms"})
			}
			objects := []client.Object{pod}
			if tc.config {
				cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-tmkms", Namespace: node.Namespace}}
				if tc.owned {
					cm.OwnerReferences = owner
				}
				objects = append(objects, cm)
			}
			recorder := record.NewFakeRecorder(10)
			r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), recorder: recorder}
			var err error
			if tc.refused {
				_, err = r.reconcileSigningConfigs(context.Background(), node)
			} else {
				err = r.refuseLegacyTmKMS(context.Background(), node)
			}
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "migrate") {
					t.Fatalf("expected migration refusal, got %v", err)
				}
				select {
				case event := <-recorder.Events:
					if !strings.Contains(event, "Warning") {
						t.Fatal(event)
					}
				default:
					t.Fatal("missing warning event")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			remaining := &corev1.Pod{}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(pod), remaining); err != nil {
				t.Fatalf("running pod was removed: %v", err)
			}
		})
	}
}
