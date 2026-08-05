package chainnode

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/chainutils"
	"github.com/voluzi/cosmopilot/v2/internal/resourcecleanup"
)

func (r *Reconciler) migrateExistingAccountSecret(ctx context.Context, chainNode *appsv1.ChainNode) error {
	if !chainNode.IsValidator() {
		return nil
	}
	return r.migrateExistingGeneratedKeySecret(ctx, chainNode, chainNode.Spec.Validator.GetAccountSecretName(chainNode))
}

// migrateExistingValidatorSecrets stamps durable attribution on the account and consensus key
// Secrets of a pre-upgrade validator. Only current controller ownership or an existing stamp
// qualifies: a key imported by the user is byte-identical to a generated one at the same name, so
// adopting on name and payload shape would place user-supplied key material under
// .spec.deletionPolicy.generatedKeys: Delete. Unowned Secrets stay retained instead.
func (r *Reconciler) migrateExistingValidatorSecrets(ctx context.Context, chainNode *appsv1.ChainNode) error {
	if !chainNode.IsValidator() {
		return nil
	}
	if err := r.migrateExistingAccountSecret(ctx, chainNode); err != nil {
		return err
	}
	return r.migrateExistingGeneratedKeySecret(ctx, chainNode, chainNode.Spec.Validator.GetPrivKeySecretName(chainNode))
}

func (r *Reconciler) migrateExistingGeneratedKeySecret(ctx context.Context, chainNode *appsv1.ChainNode, name string) error {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: chainNode.GetNamespace(),
		Name:      name,
	}, secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	managed, changed, err := resourcecleanup.PrepareGeneratedResource(
		secret, chainNode, r.Scheme, resourcecleanup.ClassGeneratedKeys, false,
	)
	if err != nil || !managed || !changed {
		return err
	}
	return r.Update(ctx, secret)
}

func (r *Reconciler) ensureAccount(ctx context.Context, chainNode *appsv1.ChainNode) error {
	logger := log.FromContext(ctx)

	if chainNode.Status.ValidatorAddress != "" {
		return nil
	}

	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: chainNode.GetNamespace(),
		Name:      chainNode.Spec.Validator.GetAccountSecretName(chainNode),
	}, secret)

	mustCreate := false
	if err != nil {
		if errors.IsNotFound(err) {
			mustCreate = true
			secret = &corev1.Secret{
				TypeMeta: metav1.TypeMeta{},
				ObjectMeta: metav1.ObjectMeta{
					Name:      chainNode.Spec.Validator.GetAccountSecretName(chainNode),
					Namespace: chainNode.GetNamespace(),
					Labels:    WithChainNodeLabels(chainNode),
				},
				Data: make(map[string][]byte),
			}
		} else {
			return err
		}
	}
	if mustCreate {
		if _, _, err := resourcecleanup.PrepareGeneratedResource(secret, chainNode, r.Scheme, resourcecleanup.ClassGeneratedKeys, true); err != nil {
			return err
		}
	}

	// Ensure private key
	var validatorAddress, accountAddress string
	mustUpdate := false
	if !mustCreate {
		_, metadataChanged, err := resourcecleanup.PrepareGeneratedResource(secret, chainNode, r.Scheme, resourcecleanup.ClassGeneratedKeys, false)
		if err != nil {
			return err
		}
		mustUpdate = metadataChanged
	}
	if _, ok := secret.Data[MnemonicKey]; !ok {
		if !mustCreate {
			mustUpdate = true
		}
		account, err := chainutils.CreateAccount(
			chainNode.Spec.Validator.GetAccountPrefix(),
			chainNode.Spec.Validator.GetValPrefix(),
			chainNode.Spec.Validator.GetAccountHDPath(),
		)
		if err != nil {
			return err
		}
		secret.Data[MnemonicKey] = []byte(account.Mnemonic)
		validatorAddress = account.ValidatorAddress
		accountAddress = account.Address
	} else {
		account, err := chainutils.AccountFromMnemonic(
			string(secret.Data[MnemonicKey]),
			chainNode.Spec.Validator.GetAccountPrefix(),
			chainNode.Spec.Validator.GetValPrefix(),
			chainNode.Spec.Validator.GetAccountHDPath(),
		)
		if err != nil {
			return err
		}
		validatorAddress = account.ValidatorAddress
		accountAddress = account.Address
		logger.Info("account imported from secret", "secret", secret.GetName())
		r.recorder.Eventf(chainNode,
			corev1.EventTypeNormal,
			appsv1.ReasonAccountImported,
			"Validator account imported from Secret",
		)
	}

	if mustCreate {
		logger.Info("creating secret with account mnemonic", "secret", secret.GetName())
		if err := r.Create(ctx, secret); err != nil {
			return err
		}
		r.recorder.Eventf(chainNode,
			corev1.EventTypeNormal,
			appsv1.ReasonAccountCreated,
			"Validator account created",
		)
	} else if mustUpdate {
		logger.Info("updating secret with account mnemonic", "secret", secret.GetName())
		if err := r.Update(ctx, secret); err != nil {
			return err
		}
	}

	// update status
	if chainNode.Status.ValidatorAddress != validatorAddress || chainNode.Status.AccountAddress != accountAddress {
		logger.Info("updating .status.validatorAddress and .status.accountAddress", "val", validatorAddress, "acc", accountAddress)
		chainNode.Status.ValidatorAddress = validatorAddress
		chainNode.Status.AccountAddress = accountAddress
		return r.Status().Update(ctx, chainNode)
	}
	return nil
}
