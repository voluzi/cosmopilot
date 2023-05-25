package v1

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

const (
	defaultPersistenceSize = "50Gi"
	defaultImageVersion    = "latest"
)

func (chainNode *ChainNode) GetPersistenceSize() string {
	if chainNode.Spec.Persistence != nil && chainNode.Spec.Persistence.Size != nil {
		return *chainNode.Spec.Persistence.Size
	}
	return defaultPersistenceSize
}

// GetPersistenceStorageClass returns the configured storage class to be used in pvc, or nil if not specified.
func (chainNode *ChainNode) GetPersistenceStorageClass() *string {
	if chainNode.Spec.Persistence == nil {
		return nil
	}
	return chainNode.Spec.Persistence.StorageClassName
}

// GetImage returns the versioned image to be used
func (chainNode *ChainNode) GetImage() string {
	version := defaultImageVersion
	if chainNode.Spec.App.Version != nil {
		version = *chainNode.Spec.App.Version
	}
	return fmt.Sprintf("%s:%s", chainNode.Spec.App.Image, version)
}

// GetImagePullPolicy returns the pull policy to be used for the app image
func (chainNode *ChainNode) GetImagePullPolicy() corev1.PullPolicy {
	if chainNode.Spec.App.ImagePullPolicy != "" {
		return chainNode.Spec.App.ImagePullPolicy
	}
	if chainNode.Spec.App.Version != nil && *chainNode.Spec.App.Version == defaultImageVersion {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}
