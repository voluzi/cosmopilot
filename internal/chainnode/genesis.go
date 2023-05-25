package chainnode

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/NibiruChain/nibiru-operator/api/v1"
	"github.com/NibiruChain/nibiru-operator/internal/chainutils"
	"github.com/NibiruChain/nibiru-operator/internal/utils"
)

func (r *Reconciler) ensureGenesis(ctx context.Context, chainNode *appsv1.ChainNode) error {
	logger := log.FromContext(ctx)

	// Return if we have a chain ID already
	if chainNode.Status.ChainID != "" {
		return nil
	}

	genesis := ""
	chainID := ""
	var err error
	if chainNode.Spec.Genesis.Url != nil {
		logger.Info("retrieving genesis from url", "url", *chainNode.Spec.Genesis.Url)
		genesis, err = utils.FetchJson(*chainNode.Spec.Genesis.Url)
		if err != nil {
			return err
		}
		chainID, err = chainutils.ExtractChainIdFromGenesis(genesis)
		if err != nil {
			return err
		}
	}

	// TODO: add other methods for retrieving genesis

	if genesis == "" || chainID == "" {
		return fmt.Errorf("genesis could not be retrived using any of the available methods")
	}

	// We create the genesis once only
	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-genesis", chainID),
			Namespace: chainNode.Namespace,
		},
		Data: map[string]string{genesisFilename: genesis},
	}
	if err := controllerutil.SetControllerReference(chainNode, cm, r.Scheme); err != nil {
		return err
	}

	logger.Info("creating genesis configmap")
	if err := r.Create(ctx, cm); err != nil {
		return err
	}

	// update chainID in status
	logger.Info("updating status with chain id")
	chainNode.Status.ChainID = chainID
	return r.Status().Update(ctx, chainNode)
}
