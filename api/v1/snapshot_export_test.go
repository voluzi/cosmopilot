package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/voluzi/cosmopilot/v2/pkg/dataexporter"
)

func TestExportTarballConfigValidate(t *testing.T) {
	gcs := &GcsExportConfig{
		Bucket:             "gcs-snapshots",
		ServiceAccountName: ptr.To("gcs-exporter"),
	}
	s3 := &S3ExportConfig{
		Bucket: "s3-snapshots",
		Region: "eu-west-1",
	}

	tests := []struct {
		name        string
		config      *ExportTarballConfig
		wantErr     bool
		errContains string
	}{
		{
			name:   "gcs destination",
			config: &ExportTarballConfig{GCS: gcs},
		},
		{
			name:   "s3 destination",
			config: &ExportTarballConfig{S3: s3},
		},
		{
			name:        "missing destination",
			config:      &ExportTarballConfig{},
			wantErr:     true,
			errContains: "one of gcs or s3 must be set",
		},
		{
			name:        "multiple destinations",
			config:      &ExportTarballConfig{GCS: gcs, S3: s3},
			wantErr:     true,
			errContains: "gcs and s3 are mutually exclusive",
		},
		{
			name:        "unsupported compression",
			config:      &ExportTarballConfig{S3: s3, Compression: ptr.To(TarballCompression("brotli"))},
			wantErr:     true,
			errContains: "unsupported compression",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate(".spec.persistence.snapshots.exportTarball")
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestExportTarballCompressionDefaultsToGzip(t *testing.T) {
	config := &ExportTarballConfig{}
	assert.Equal(t, dataexporter.CompressionGzip, config.GetCompression())

	config.Compression = ptr.To(TarballCompression(dataexporter.CompressionLz4))
	assert.Equal(t, dataexporter.CompressionLz4, config.GetCompression())
}

func TestS3ExportConfigValidate(t *testing.T) {
	tests := []struct {
		name        string
		config      *S3ExportConfig
		wantErr     bool
		errContains string
	}{
		{
			name:   "default credential chain",
			config: &S3ExportConfig{Bucket: "snapshots", Region: "eu-west-1"},
		},
		{
			name:        "bucket is required",
			config:      &S3ExportConfig{Region: "eu-west-1"},
			wantErr:     true,
			errContains: "bucket must not be empty",
		},
		{
			name:        "region is required",
			config:      &S3ExportConfig{Bucket: "snapshots"},
			wantErr:     true,
			errContains: "region must not be empty",
		},
		{
			name: "access key secret",
			config: &S3ExportConfig{
				Bucket:            "snapshots",
				Region:            "eu-west-1",
				CredentialsSecret: &corev1.LocalObjectReference{Name: "aws-credentials"},
			},
		},
		{
			name: "irsa service account",
			config: &S3ExportConfig{
				Bucket:             "snapshots",
				Region:             "eu-west-1",
				ServiceAccountName: ptr.To("snapshot-exporter"),
			},
		},
		{
			name: "minio endpoint",
			config: &S3ExportConfig{
				Bucket:         "snapshots",
				Region:         "us-east-1",
				Endpoint:       ptr.To("http://minio.storage.svc:9000"),
				ForcePathStyle: ptr.To(true),
			},
		},
		{
			name: "credentials and service account are mutually exclusive",
			config: &S3ExportConfig{
				Bucket:             "snapshots",
				Region:             "eu-west-1",
				CredentialsSecret:  &corev1.LocalObjectReference{Name: "aws-credentials"},
				ServiceAccountName: ptr.To("snapshot-exporter"),
			},
			wantErr:     true,
			errContains: "mutually exclusive",
		},
		{
			name:        "empty service account",
			config:      &S3ExportConfig{Bucket: "snapshots", Region: "eu-west-1", ServiceAccountName: ptr.To("")},
			wantErr:     true,
			errContains: "serviceAccountName must not be empty",
		},
		{
			name:        "unsupported endpoint scheme",
			config:      &S3ExportConfig{Bucket: "snapshots", Region: "us-east-1", Endpoint: ptr.To("ftp://minio.example.com")},
			wantErr:     true,
			errContains: "endpoint must use http or https",
		},
		{
			name:        "invalid endpoint",
			config:      &S3ExportConfig{Bucket: "snapshots", Region: "us-east-1", Endpoint: ptr.To("://bad")},
			wantErr:     true,
			errContains: "endpoint is invalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate(".spec.persistence.snapshots.exportTarball.s3")
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
				return
			}
			assert.NoError(t, err)
		})
	}
}
