package cosmopilot

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

var imageOverrideEnvNames = map[string]string{
	"nodeUtilsImage":         "NODE_UTILS_IMAGE",
	"cosmoGuardImage":        "COSMOGUARD_IMAGE",
	"cosmoseedImage":         "COSMOSEED_IMAGE",
	"cosmosignerImage":       "COSMOSIGNER_IMAGE",
	"dataExporterImage":      "DATA_EXPORTER_IMAGE",
	"utilityImage":           "UTILITY_IMAGE",
	"tmkmsImage":             "TMKMS_IMAGE",
	"vaultTokenRenewerImage": "VAULT_TOKEN_RENEWER_IMAGE",
}

func TestCompanionImageValuesDefaultToManagerDefaults(t *testing.T) {
	valuesSource, err := os.ReadFile("values.yaml")
	require.NoError(t, err)

	var values map[string]any
	require.NoError(t, yaml.Unmarshal(valuesSource, &values))
	for key := range imageOverrideEnvNames {
		value, ok := values[key]
		assert.True(t, ok, "%s must remain a supported value", key)
		assert.Equal(t, "", value, "%s must inherit the manager default", key)
	}
}

func TestDeploymentOmitsEmptyImageOverrides(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
	}{
		{name: "values defaults", values: defaultChartValues()},
		{name: "keys omitted", values: baseChartValues()},
		{name: "keys null", values: imageValues(nil)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deployment := renderDeployment(t, tt.values, "3.0.0-beta.7")
			for _, envName := range imageOverrideEnvNames {
				_, ok := deployment.env[envName]
				assert.False(t, ok, "%s must be omitted", envName)
			}
		})
	}
}

func TestDeploymentPassesThroughEachImageOverride(t *testing.T) {
	images := []string{
		"registry.example.com:5000/image:custom",
		"registry.example.com/image@sha256:abcdef",
		"true",
	}

	for key, envName := range imageOverrideEnvNames {
		for _, image := range images {
			t.Run(key+"/"+image, func(t *testing.T) {
				values := defaultChartValues()
				values[key] = image
				deployment := renderDeployment(t, values, "3.0.0-beta.7")
				require.Equal(t, image, deployment.env[envName].Value)
				require.Equal(t, "!!str", deployment.env[envName].Tag)
				for otherKey, otherEnvName := range imageOverrideEnvNames {
					if otherKey != key {
						_, ok := deployment.env[otherEnvName]
						assert.False(t, ok, "%s must not configure %s", key, otherEnvName)
					}
				}
			})
		}
	}
}

func TestDeploymentPassesThroughAllImageOverrides(t *testing.T) {
	values := defaultChartValues()
	for key := range imageOverrideEnvNames {
		values[key] = fmt.Sprintf("registry.example.com/%s:custom", key)
	}

	deployment := renderDeployment(t, values, "3.0.0-beta.7")
	for key, envName := range imageOverrideEnvNames {
		assert.Equal(t, values[key], deployment.env[envName].Value)
	}
}

func TestManagerImageUsesAppVersionUnlessImageTagIsSet(t *testing.T) {
	values := defaultChartValues()
	assert.Equal(t, "ghcr.io/voluzi/cosmopilot:3.0.0-beta.7", renderDeployment(t, values, "3.0.0-beta.7").image)

	values["imageTag"] = "3.1.0-custom"
	assert.Equal(t, "ghcr.io/voluzi/cosmopilot:3.1.0-custom", renderDeployment(t, values, "3.0.0-beta.7").image)
}

type renderedDeployment struct {
	image string
	env   map[string]yaml.Node
}

func renderDeployment(t *testing.T, values map[string]any, appVersion string) renderedDeployment {
	t.Helper()
	templateSource, err := os.ReadFile("templates/deployment.yaml")
	require.NoError(t, err)

	chartTemplate, err := template.New("deployment.yaml").Funcs(template.FuncMap{
		"include": func(string, any) string {
			return "\napp.kubernetes.io/name: cosmopilot\napp.kubernetes.io/instance: test"
		},
		"indent": func(spaces int, value string) string {
			prefix := strings.Repeat(" ", spaces)
			return prefix + strings.ReplaceAll(value, "\n", "\n"+prefix)
		},
		"randAlphaNum": func(int) string { return "abcde" },
		"quote":        func(value any) string { return fmt.Sprintf("%q", fmt.Sprint(value)) },
		"ternary": func(trueValue, falseValue string, condition bool) string {
			if condition {
				return trueValue
			}
			return falseValue
		},
		"toYaml": func(value any) string {
			encoded, marshalErr := yaml.Marshal(value)
			require.NoError(t, marshalErr)
			return string(encoded)
		},
	}).Parse(string(templateSource))
	require.NoError(t, err)

	data := map[string]any{
		"Release": map[string]any{"Name": "test", "Namespace": "default"},
		"Chart":   map[string]any{"AppVersion": appVersion},
		"Values":  values,
	}
	var rendered bytes.Buffer
	require.NoError(t, chartTemplate.Execute(&rendered, data))

	var deployment struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Image string `yaml:"image"`
						Env   []struct {
							Name  string    `yaml:"name"`
							Value yaml.Node `yaml:"value"`
						} `yaml:"env"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	require.NoErrorf(t, yaml.Unmarshal(rendered.Bytes(), &deployment), "%s", rendered.String())
	require.Len(t, deployment.Spec.Template.Spec.Containers, 1)

	env := make(map[string]yaml.Node)
	for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
		env[variable.Name] = variable.Value
	}
	return renderedDeployment{image: deployment.Spec.Template.Spec.Containers[0].Image, env: env}
}

func baseChartValues() map[string]any {
	return map[string]any{
		"replicas":                1,
		"image":                   "ghcr.io/voluzi/cosmopilot",
		"imagePullSecrets":        []string{},
		"webHooksEnabled":         false,
		"nodeSelector":            map[string]string{},
		"workerName":              "",
		"workerCount":             10,
		"debugMode":               false,
		"disruptionChecksEnabled": true,
		"probesEnabled":           false,
	}
}

func defaultChartValues() map[string]any {
	values := baseChartValues()
	for key := range imageOverrideEnvNames {
		values[key] = ""
	}
	return values
}

func imageValues(value any) map[string]any {
	values := baseChartValues()
	for key := range imageOverrideEnvNames {
		values[key] = value
	}
	return values
}
