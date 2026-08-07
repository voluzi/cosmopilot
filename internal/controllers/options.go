package controllers

import (
	"context"
	"fmt"
)

const (
	LabelWorkerName          = "worker-name"
	DefaultDataExporterImage = "ghcr.io/voluzi/dataexporter:2.0.1"
)

type ControllerRunOptions struct {
	WorkerCount              int
	WorkerName               string
	NodeUtilsImage           string
	DisableWebhooks          bool
	CosmoGuardImage          string
	CosmoseedImage           string
	CosmosignerImage         string
	DataExporterImage        string
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
		return DefaultDataExporterImage
	}
	return opts.DataExporterImage
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
