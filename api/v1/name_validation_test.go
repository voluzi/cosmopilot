package v1

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
)

func TestChainNodeValidateRejectsInvalidNamesAndDurations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*ChainNode)
		wantErr string
	}{
		{name: "valid"},
		{
			name:    "app binary path",
			mutate:  func(c *ChainNode) { c.Spec.App.App = "/usr/local/bin/gaiad" },
			wantErr: ".spec.app.app",
		},
		{
			name:    "uppercase app binary",
			mutate:  func(c *ChainNode) { c.Spec.App.App = "Gaiad" },
			wantErr: ".spec.app.app",
		},
		{
			name: "sidecar name with spaces",
			mutate: func(c *ChainNode) {
				c.Spec.Config = &Config{Sidecars: []SidecarSpec{{Name: "My Sidecar"}}}
			},
			wantErr: ".spec.config.sidecars[0].name",
		},
		{
			name: "additional volume named like a built-in volume",
			mutate: func(c *ChainNode) {
				c.Spec.Persistence = &Persistence{AdditionalVolumes: []VolumeSpec{{Name: "data", Size: "1Gi", Path: "/extra"}}}
			},
			wantErr: "collides with a built-in pod volume",
		},
		{
			name: "additional volume named like a TmKMS volume",
			mutate: func(c *ChainNode) {
				c.Spec.Persistence = &Persistence{AdditionalVolumes: []VolumeSpec{{Name: "tmkms-config", Size: "1Gi", Path: "/extra"}}}
			},
			wantErr: "collides with a built-in pod volume",
		},
		{
			name: "additional volume named like a data-init volume",
			mutate: func(c *ChainNode) {
				c.Spec.Persistence = &Persistence{AdditionalVolumes: []VolumeSpec{{Name: "temp", Size: "1Gi", Path: "/extra"}}}
			},
			wantErr: "collides with a built-in pod volume",
		},
		{
			name: "additional volume with an invalid name",
			mutate: func(c *ChainNode) {
				c.Spec.Persistence = &Persistence{AdditionalVolumes: []VolumeSpec{{Name: "Extra_Vol", Size: "1Gi", Path: "/extra"}}}
			},
			wantErr: ".spec.persistence.additionalVolumes[0].name",
		},
		{
			name: "duplicate additional volume",
			mutate: func(c *ChainNode) {
				c.Spec.Persistence = &Persistence{AdditionalVolumes: []VolumeSpec{
					{Name: "extra", Size: "1Gi", Path: "/a"}, {Name: "extra", Size: "1Gi", Path: "/b"},
				}}
			},
			wantErr: "duplicates",
		},
		{
			name: "sidecar named like a built-in container",
			mutate: func(c *ChainNode) {
				c.Spec.Config = &Config{Sidecars: []SidecarSpec{{Name: "node-utils"}}}
			},
			wantErr: "collides with a built-in pod container",
		},
		{
			name: "sidecar named like the app container",
			mutate: func(c *ChainNode) {
				c.Spec.Config = &Config{Sidecars: []SidecarSpec{{Name: "chaind"}}}
			},
			wantErr: "collides with a built-in pod container",
		},
		{
			name: "duplicate sidecar names",
			mutate: func(c *ChainNode) {
				c.Spec.Config = &Config{Sidecars: []SidecarSpec{{Name: "exporter"}, {Name: "exporter"}}}
			},
			wantErr: ".spec.config.sidecars[1].name",
		},
		{
			name: "negative additional volume size",
			mutate: func(c *ChainNode) {
				c.Spec.Persistence = &Persistence{AdditionalVolumes: []VolumeSpec{{Name: "extra", Size: "-1Gi", Path: "/extra"}}}
			},
			wantErr: "must be greater than zero",
		},
		{
			name: "malformed additional volume size",
			mutate: func(c *ChainNode) {
				c.Spec.Persistence = &Persistence{AdditionalVolumes: []VolumeSpec{{Name: "extra", Size: "ten gigs", Path: "/extra"}}}
			},
			wantErr: "bad format for .spec.persistence.additionalVolumes[0].size",
		},
		{
			name: "genesis duration in days",
			mutate: func(c *ChainNode) {
				c.Spec.Genesis = nil
				c.Spec.Validator = &ValidatorConfig{Init: &GenesisInitConfig{
					ChainID: "chain-1", Assets: []string{"1stake"}, StakeAmount: "1stake", UnbondingTime: ptr.To("21d"),
				}}
			},
			wantErr: ".spec.validator.init.unbondingTime",
		},
		{
			name: "genesis duration in words",
			mutate: func(c *ChainNode) {
				c.Spec.Genesis = nil
				c.Spec.Validator = &ValidatorConfig{Init: &GenesisInitConfig{
					ChainID: "chain-1", Assets: []string{"1stake"}, StakeAmount: "1stake", VotingPeriod: ptr.To("3 weeks"),
				}}
			},
			wantErr: ".spec.validator.init.votingPeriod",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chainNode := validChainNodeForDeletionPolicyTest()
			if tc.mutate != nil {
				tc.mutate(chainNode)
			}
			_, err := chainNode.Validate(nil)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestChainNodeValidateAdmitsAdditionalVolumeResize(t *testing.T) {
	withVolume := func(size string) *ChainNode {
		c := validChainNodeForDeletionPolicyTest()
		c.Spec.Persistence = &Persistence{AdditionalVolumes: []VolumeSpec{{Name: "extra", Size: size, Path: "/extra"}}}
		return c
	}
	// A decrease must stay possible: it may revert an expansion the storage class refused. The
	// controller skips a real shrink with an event.
	_, err := withVolume("20Gi").Validate(withVolume("30Gi"))
	require.NoError(t, err)
	_, err = withVolume("30Gi").Validate(withVolume("20Gi"))
	require.NoError(t, err)
}

func TestChainNodeSetValidateRejectsInvalidNames(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*ChainNodeSet)
		wantErr string
	}{
		{name: "valid", mutate: func(s *ChainNodeSet) { s.Spec.Nodes = []NodeGroupSpec{{Name: "fullnodes"}} }},
		{
			name:    "uppercase group name",
			mutate:  func(s *ChainNodeSet) { s.Spec.Nodes = []NodeGroupSpec{{Name: "FullNodes"}} },
			wantErr: ".spec.nodes[0].name",
		},
		{
			name:    "underscore group name",
			mutate:  func(s *ChainNodeSet) { s.Spec.Nodes = []NodeGroupSpec{{Name: "public_api"}} },
			wantErr: ".spec.nodes[0].name",
		},
		{
			name: "group sidecar name",
			mutate: func(s *ChainNodeSet) {
				s.Spec.Nodes = []NodeGroupSpec{{Name: "fullnodes", Config: &Config{Sidecars: []SidecarSpec{{Name: "Side Car"}}}}}
			},
			wantErr: ".spec.nodes[0].config.sidecars[0].name",
		},
		{
			name: "group validator additional volume named like a built-in volume",
			mutate: func(s *ChainNodeSet) {
				s.Spec.Nodes = []NodeGroupSpec{{Name: "validators", Validator: &NodeSetValidatorConfig{
					Persistence: &Persistence{AdditionalVolumes: []VolumeSpec{{Name: "config", Size: "1Gi", Path: "/x"}}},
				}}}
			},
			wantErr: ".spec.nodes[0].validator.persistence.additionalVolumes[0].name",
		},
		{
			name: "validator group ignores its unused group-level persistence and config",
			mutate: func(s *ChainNodeSet) {
				s.Spec.Nodes = []NodeGroupSpec{{
					Name:        "validators",
					Validator:   &NodeSetValidatorConfig{},
					Persistence: &Persistence{AdditionalVolumes: []VolumeSpec{{Name: "data", Size: "1Gi", Path: "/x"}}},
					Config:      &Config{Sidecars: []SidecarSpec{{Name: "Side Car"}}},
				}}
			},
		},
		{
			name: "global ingress name",
			mutate: func(s *ChainNodeSet) {
				s.Spec.Nodes = []NodeGroupSpec{{Name: "fullnodes"}}
				s.Spec.Ingresses = []GlobalIngressConfig{{Name: "Public API", Groups: []string{"fullnodes"}}}
			},
			wantErr: ".spec.ingresses[0].name",
		},
		{
			name: "gateway route name",
			mutate: func(s *ChainNodeSet) {
				s.Spec.Nodes = []NodeGroupSpec{{Name: "fullnodes"}}
				s.Spec.GatewayRoutes = []GlobalGatewayConfig{{Name: "Public_API", Groups: []string{"fullnodes"}}}
			},
			wantErr: ".spec.gatewayRoutes[0].name",
		},
		{
			name:    "app binary path",
			mutate:  func(s *ChainNodeSet) { s.Spec.App.App = "/usr/local/bin/gaiad" },
			wantErr: ".spec.app.app",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodeSet := validChainNodeSetForDeletionPolicyTest()
			tc.mutate(nodeSet)
			_, err := nodeSet.Validate(nil)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}
