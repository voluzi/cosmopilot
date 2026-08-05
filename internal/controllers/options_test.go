package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestGetDataExporterImage(t *testing.T) {
	tests := []struct {
		name  string
		image string
		want  string
	}{
		{name: "default", want: DefaultDataExporterImage},
		{name: "configured", image: "registry.example.com/dataexporter:custom", want: "registry.example.com/dataexporter:custom"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := &ControllerRunOptions{DataExporterImage: tt.image}
			assert.Equal(t, tt.want, opts.GetDataExporterImage())
		})
	}
}

func TestGetDataExporterImageWithNilOptions(t *testing.T) {
	var opts *ControllerRunOptions
	assert.Equal(t, DefaultDataExporterImage, opts.GetDataExporterImage())
}

func TestWaitForRootProtectionBlocksUntilMigrationCompletes(t *testing.T) {
	ready := make(chan struct{})
	opts := &ControllerRunOptions{RootProtectionReady: ready}
	done := make(chan error, 1)
	go func() { done <- opts.WaitForRootProtection(context.Background()) }()

	select {
	case err := <-done:
		t.Fatalf("controller gate opened before migration completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(ready)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
