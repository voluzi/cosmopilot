package cosmopilot

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v3"
)

func TestHelperImageDefaults(t *testing.T) {
	valuesSource, err := os.ReadFile("values.yaml")
	if err != nil {
		t.Fatalf("read values: %v", err)
	}

	var values struct {
		DataExporterImage string `yaml:"dataExporterImage"`
		UtilityImage      string `yaml:"utilityImage"`
	}
	if err := yaml.Unmarshal(valuesSource, &values); err != nil {
		t.Fatalf("decode values: %v", err)
	}
	if values.DataExporterImage != "ghcr.io/voluzi/dataexporter:2.0.1" {
		t.Errorf("dataExporterImage = %q, want %q", values.DataExporterImage, "ghcr.io/voluzi/dataexporter:2.0.1")
	}
	if values.UtilityImage != "ghcr.io/voluzi/node-tools:1.4.3" {
		t.Errorf("utilityImage = %q, want %q", values.UtilityImage, "ghcr.io/voluzi/node-tools:1.4.3")
	}
}

func TestDeploymentConfiguresHelperImagesAsStrings(t *testing.T) {
	templateSource, err := os.ReadFile("templates/deployment.yaml")
	if err != nil {
		t.Fatalf("read deployment template: %v", err)
	}

	chartTemplate, err := template.New("deployment.yaml").Funcs(template.FuncMap{
		"include": func(string, any) string {
			return "\napp.kubernetes.io/name: cosmopilot\napp.kubernetes.io/instance: test"
		},
		"indent": func(spaces int, value string) string {
			prefix := strings.Repeat(" ", spaces)
			return prefix + strings.ReplaceAll(value, "\n", "\n"+prefix)
		},
		"randAlphaNum": func(int) string { return "abcde" },
		"quote":        func(value string) string { return `"` + value + `"` },
		"ternary": func(trueValue, falseValue string, condition bool) string {
			if condition {
				return trueValue
			}
			return falseValue
		},
		"toYaml": func(value any) string {
			encoded, marshalErr := yaml.Marshal(value)
			if marshalErr != nil {
				t.Fatalf("encode template value: %v", marshalErr)
			}
			return string(encoded)
		},
	}).Parse(string(templateSource))
	if err != nil {
		t.Fatalf("parse deployment template: %v", err)
	}

	const image = "true"
	data := map[string]any{
		"Release": map[string]any{"Name": "test", "Namespace": "default"},
		"Chart":   map[string]any{"AppVersion": "3.0.0-beta.7"},
		"Values": map[string]any{
			"replicas":                1,
			"image":                   "ghcr.io/voluzi/cosmopilot",
			"imageTag":                "3.0.0-beta.7",
			"imagePullSecrets":        []string{},
			"webHooksEnabled":         false,
			"nodeSelector":            map[string]string{},
			"nodeUtilsImage":          "node-utils",
			"cosmoGuardImage":         "cosmoguard",
			"cosmoseedImage":          "cosmoseed",
			"cosmosignerImage":        "cosmosigner",
			"dataExporterImage":       image,
			"utilityImage":            image,
			"workerName":              "",
			"workerCount":             10,
			"debugMode":               false,
			"disruptionChecksEnabled": true,
			"probesEnabled":           false,
		},
	}

	var rendered bytes.Buffer
	if err := chartTemplate.Execute(&rendered, data); err != nil {
		t.Fatalf("render deployment template: %v", err)
	}

	var deployment struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Env []struct {
							Name  string    `yaml:"name"`
							Value yaml.Node `yaml:"value"`
						} `yaml:"env"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(rendered.Bytes(), &deployment); err != nil {
		t.Fatalf("decode rendered deployment: %v\n%s", err, rendered.String())
	}

	want := map[string]bool{"DATA_EXPORTER_IMAGE": false, "UTILITY_IMAGE": false}
	for _, env := range deployment.Spec.Template.Spec.Containers[0].Env {
		if _, ok := want[env.Name]; ok {
			if env.Value.Tag != "!!str" || env.Value.Value != image {
				t.Errorf("%s = %s %q, want !!str %q", env.Name, env.Value.Tag, env.Value.Value, image)
			}
			want[env.Name] = true
		}
	}
	for name, configured := range want {
		if !configured {
			t.Errorf("%s is not configured", name)
		}
	}
}
