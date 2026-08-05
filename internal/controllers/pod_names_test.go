package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsDeterministicChainNodePodName(t *testing.T) {
	nodeName := "validator"
	for _, podName := range []string{
		nodeName,
		nodeName + "-init-data",
		nodeName + "-config-generator",
		nodeName + "-genesis-init",
		nodeName + "-create-validator",
		nodeName + "-tmkms-generate-identity",
		nodeName + "-tmkms-vault-upload",
	} {
		assert.True(t, IsDeterministicChainNodePodName(podName, nodeName), podName)
	}

	for _, podName := range []string{
		"other-init-data",
		nodeName + "-init-data-extra",
		nodeName + "-unknown-helper",
		"",
	} {
		assert.False(t, IsDeterministicChainNodePodName(podName, nodeName), podName)
	}
}
