package chainnodeset

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	appsv1 "github.com/voluzi/cosmopilot/api/v1"
)

const (
	ChainNodeSetPrefix = "spo-chainnodeset-e2e-"
)

var (
	DefaultTestApp = appsv1.AppSpec{
		Image:   "ghcr.io/nibiruchain/nibiru",
		Version: ptr.To("1.0.0"),
		App:     "nibid",
	}
)

func NewChainNodeSetBasic(ns *corev1.Namespace, app appsv1.AppSpec) *appsv1.ChainNodeSet {
	return &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: ChainNodeSetPrefix,
			Namespace:    ns.Name,
		},
		Spec: appsv1.ChainNodeSetSpec{
			App: app,
		},
	}
}
