package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func init() {
	SchemeBuilder.Register(&ConsensusKeyReservation{}, &ConsensusKeyReservationList{})
}

// ConsensusKeyReservationSpec records the controller root and logical claim allowed to manage one
// consensus public key on one chain. The owning root's reservation lifecycle finalizer releases the
// immutable reservation only after all attributable signing workloads are confirmed absent.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="consensus key reservations are immutable; release requires verified signing-path teardown"
type ConsensusKeyReservationSpec struct {
	ChainID   string `json:"chainID"`
	PublicKey string `json:"publicKey"`
	// OwnerUID identifies the controller root that owns this reservation for conflict detection.
	OwnerUID  types.UID `json:"ownerUID"`
	OwnerKind string    `json:"ownerKind"`
	Namespace string    `json:"namespace"`
	OwnerName string    `json:"ownerName"`
	Claim     string    `json:"claim"`
}

// +kubebuilder:object:root=true

// ConsensusKeyReservationList contains consensus-key reservations.
type ConsensusKeyReservationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ConsensusKeyReservation `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ckr
// +kubebuilder:printcolumn:name="ChainID",type=string,JSONPath=`.spec.chainID`
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.ownerName`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.namespace`

// ConsensusKeyReservation atomically prevents independent roots or claims from managing separate
// double-sign state for the same chain and consensus public key.
type ConsensusKeyReservation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ConsensusKeyReservationSpec `json:"spec"`
}
