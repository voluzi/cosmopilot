package controllers

import (
	"context"
	"fmt"

	"github.com/voluzi/cosmopilot/v4/pkg/images"
)

const LabelWorkerName = "worker-name"

type ControllerRunOptions struct {
	WorkerCount              int
	WorkerName               string
	NodeUtilsImage           string
	DisableWebhooks          bool
	CosmoGuardImage          string
	CosmoseedImage           string
	CosmosignerImage         string
	DataExporterImage        string
	UtilityImage             string
	TmKmsImage               string
	VaultTokenRenewerImage   string
	ReleaseName              string
	DisruptionCheckEnabled   bool
	DisruptionMaxUnavailable int
	RootProtectionReady      <-chan struct{}
}

func (opts *ControllerRunOptions) WaitForRootProtection(ctx context.Context) error {
	if opts == nil || opts.RootProtectionReady == nil {
		return nil
	}
	select {
	case <-opts.RootProtectionReady:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (opts *ControllerRunOptions) MatchesWorker(labels map[string]string) bool {
	workerName := ""
	if opts != nil {
		workerName = opts.WorkerName
	}
	return MatchesWorker(labels, workerName)
}

func MatchesWorker(labels map[string]string, workerName string) bool {
	return labels[LabelWorkerName] == workerName
}

func (opts *ControllerRunOptions) GetDataExporterImage() string {
	if opts == nil || opts.DataExporterImage == "" {
		return images.DefaultDataExporterImage
	}
	return opts.DataExporterImage
}

func (opts *ControllerRunOptions) GetUtilityImage() string {
	if opts == nil || opts.UtilityImage == "" {
		return images.DefaultUtilityImage
	}
	return opts.UtilityImage
}

func (opts *ControllerRunOptions) GetNodeUtilsImage() string {
	if opts == nil || opts.NodeUtilsImage == "" {
		return images.DefaultNodeUtilsImage
	}
	return opts.NodeUtilsImage
}

func (opts *ControllerRunOptions) GetCosmoseedImage() string {
	if opts == nil || opts.CosmoseedImage == "" {
		return images.DefaultCosmoseedImage
	}
	return opts.CosmoseedImage
}

func (opts *ControllerRunOptions) GetCosmoGuardImage() string {
	if opts == nil || opts.CosmoGuardImage == "" {
		return images.DefaultCosmoGuardImage
	}
	return opts.CosmoGuardImage
}

func (opts *ControllerRunOptions) GetCosmosignerImage() string {
	if opts == nil || opts.CosmosignerImage == "" {
		return images.DefaultCosmosignerImage
	}
	return opts.CosmosignerImage
}

func (opts *ControllerRunOptions) GetTmKmsImage() string {
	if opts == nil || opts.TmKmsImage == "" {
		return images.DefaultTmKmsImage
	}
	return opts.TmKmsImage
}

func (opts *ControllerRunOptions) GetVaultTokenRenewerImage() string {
	if opts == nil || opts.VaultTokenRenewerImage == "" {
		return images.DefaultVaultTokenRenewerImage
	}
	return opts.VaultTokenRenewerImage
}

func (opts *ControllerRunOptions) GetDefaultPriorityClassName() string {
	if opts.ReleaseName == "" {
		return ""
	}
	return fmt.Sprintf("%s-default", opts.ReleaseName)
}

func (opts *ControllerRunOptions) GetNodesPriorityClassName() string {
	if opts.ReleaseName == "" {
		return ""
	}
	return fmt.Sprintf("%s-nodes", opts.ReleaseName)
}

func (opts *ControllerRunOptions) GetValidatorsPriorityClassName() string {
	if opts.ReleaseName == "" {
		return ""
	}
	return fmt.Sprintf("%s-validators", opts.ReleaseName)
}
