package cosmopilot

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/voluzi/cosmopilot/v3/internal/controllers"
)

func TestPriorityClassResourcesMatchManagerReferences(t *testing.T) {
	for _, releaseName := range []string{"cosmopilot", "audit-release"} {
		t.Run(releaseName, func(t *testing.T) {
			renderedNames := renderPriorityClassNames(t, releaseName)
			runOpts := controllers.ControllerRunOptions{ReleaseName: releaseName}
			expectedNames := []string{
				runOpts.GetDefaultPriorityClassName(),
				runOpts.GetNodesPriorityClassName(),
				runOpts.GetValidatorsPriorityClassName(),
			}

			assert.ElementsMatch(t, expectedNames, renderedNames)
		})
	}
}

func renderPriorityClassNames(t *testing.T, releaseName string) []string {
	t.Helper()
	templateSource, err := os.ReadFile("templates/priority-classes.yaml")
	require.NoError(t, err)

	chartTemplate, err := template.New("priority-classes.yaml").Funcs(template.FuncMap{
		"include": func(string, any) string {
			return "\napp.kubernetes.io/name: cosmopilot\napp.kubernetes.io/instance: test"
		},
		"indent": func(spaces int, value string) string {
			prefix := strings.Repeat(" ", spaces)
			return prefix + strings.ReplaceAll(value, "\n", "\n"+prefix)
		},
		"lookup": func(string, string, string, string) map[string]any {
			return map[string]any{"metadata": map[string]any{"name": "existing"}}
		},
	}).Parse(string(templateSource))
	require.NoError(t, err)

	var rendered bytes.Buffer
	require.NoError(t, chartTemplate.Execute(&rendered, map[string]any{
		"Release": map[string]any{"Name": releaseName},
		"Values": map[string]any{
			"defaultPriority":      0,
			"nodesPodPriority":     950,
			"validatorPodPriority": 1050,
		},
	}))

	type manifest struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
	}

	decoder := yaml.NewDecoder(&rendered)
	var names []string
	for {
		var resource manifest
		err := decoder.Decode(&resource)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if resource.Kind != "PriorityClass" {
			continue
		}
		names = append(names, resource.Metadata.Name)
	}

	require.Len(t, names, 3)
	return names
}
