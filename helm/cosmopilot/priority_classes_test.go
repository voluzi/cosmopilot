package cosmopilot

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v3"
)

func TestPriorityClassResourcesAreUnique(t *testing.T) {
	templateSource, err := os.ReadFile("templates/priority-classes.yaml")
	if err != nil {
		t.Fatalf("read priority class template: %v", err)
	}

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
	if err != nil {
		t.Fatalf("parse priority class template: %v", err)
	}

	var rendered bytes.Buffer
	err = chartTemplate.Execute(&rendered, map[string]any{
		"Release": map[string]any{"Name": "test"},
		"Values": map[string]any{
			"defaultPriority":      0,
			"nodesPodPriority":     950,
			"validatorPodPriority": 1050,
		},
	})
	if err != nil {
		t.Fatalf("render priority class template: %v", err)
	}

	type manifest struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
	}

	decoder := yaml.NewDecoder(&rendered)
	seen := make(map[string]struct{})
	for {
		var resource manifest
		err := decoder.Decode(&resource)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode rendered manifest: %v", err)
		}
		if resource.Kind != "PriorityClass" {
			continue
		}

		identity := resource.Kind + "/" + resource.Metadata.Name
		if _, exists := seen[identity]; exists {
			t.Errorf("rendered duplicate resource %q", identity)
		}
		seen[identity] = struct{}{}
	}

	if len(seen) != 3 {
		t.Errorf("rendered %d unique priority classes, want 3", len(seen))
	}
}
