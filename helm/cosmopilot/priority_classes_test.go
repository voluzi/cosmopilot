package cosmopilot

import (
	"os"
	"strings"
	"testing"
)

func TestPriorityClassResourcesAreUnique(t *testing.T) {
	template, err := os.ReadFile("templates/priority-classes.yaml")
	if err != nil {
		t.Fatalf("read priority class template: %v", err)
	}

	for _, suffix := range []string{"default", "nodes", "validators"} {
		name := "name: {{ .Release.Name }}-" + suffix
		if count := strings.Count(string(template), name); count != 1 {
			t.Errorf("priority class %q is declared %d times, want 1", suffix, count)
		}
	}
}
