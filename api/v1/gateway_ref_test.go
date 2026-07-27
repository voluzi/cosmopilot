package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGatewayRefParentReference(t *testing.T) {
	namespace := "gateway"
	section := "https-dashboard"

	ref := (GatewayRef{Name: "external", Namespace: &namespace, SectionName: &section}).GetParentRef()

	assert.Equal(t, "external", string(ref.Name))
	require.NotNil(t, ref.Namespace)
	assert.Equal(t, namespace, string(*ref.Namespace))
	require.NotNil(t, ref.SectionName)
	assert.Equal(t, section, string(*ref.SectionName))
}

func TestExposeGatewayParentReferencePreservesListener(t *testing.T) {
	namespace := "gateway"
	section := "p2p"
	port := int32(30000)
	expose := &ExposeConfig{Gateway: &ExposeGatewayConfig{
		GatewayRef: GatewayRef{Name: "external", Namespace: &namespace, SectionName: &section},
		Port:       &port,
	}}

	ref := expose.GetGatewayParentRef()

	assert.Equal(t, "external", string(ref.Name))
	require.NotNil(t, ref.Namespace)
	assert.Equal(t, namespace, string(*ref.Namespace))
	require.NotNil(t, ref.SectionName)
	assert.Equal(t, section, string(*ref.SectionName))
	require.NotNil(t, ref.Port)
	assert.Equal(t, port, int32(*ref.Port))
}
