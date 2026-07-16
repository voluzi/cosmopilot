package chainnode

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func requireSameControllerOwner(existing, desired metav1.Object, kind string) error {
	want := metav1.GetControllerOf(desired)
	got := metav1.GetControllerOf(existing)
	if want == nil {
		return fmt.Errorf("desired ChainNode %s %q has no controller owner", kind, desired.GetName())
	}
	if got == nil || got.UID != want.UID {
		return fmt.Errorf("ChainNode %s %q is managed by another owner; refusing to overwrite it", kind, existing.GetName())
	}
	return nil
}
