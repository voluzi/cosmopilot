package controllers

import "fmt"

const (
	LabelWorkerName          = "worker-name"
	DefaultDataExporterImage = "ghcr.io/voluzi/dataexporter:2.0.0"
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
