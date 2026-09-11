package workflows

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type workflow struct {
	Permissions map[string]string      `yaml:"permissions"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStep    `yaml:"steps"`
}

type workflowStep struct {
	Name string         `yaml:"name"`
	ID   string         `yaml:"id"`
	Uses string         `yaml:"uses"`
	With map[string]any `yaml:"with"`
	Run  string         `yaml:"run"`
}

func TestReleaseNotesDoNotInterpolateChangelogOutput(t *testing.T) {
	wf := loadReleaseWorkflow(t)
	generate := findStep(t, wf.Jobs["release"].Steps, "Generate changelog")
	compose := findStep(t, wf.Jobs["release"].Steps, "Compose release notes")

	if generate.ID != "" {
		t.Errorf("generate changelog step has unused output id %q", generate.ID)
	}
	if strings.Contains(generate.Run, "GITHUB_OUTPUT") {
		t.Error("generate changelog step transfers commit-controlled text through GITHUB_OUTPUT")
	}
	if strings.Contains(compose.Run, "${{ steps.changes.outputs.changes }}") {
		t.Fatal("compose step interpolates commit-controlled changelog output into shell source")
	}
}

func TestReleaseChangelogScriptsTreatCommitTextAsData(t *testing.T) {
	t.Parallel()

	wf := loadReleaseWorkflow(t)
	generate := findStep(t, wf.Jobs["release"].Steps, "Generate changelog")
	compose := findStep(t, wf.Jobs["release"].Steps, "Compose release notes")
	fixture, err := os.ReadFile(filepath.Join("testdata", "changelog.txt"))
	if err != nil {
		t.Fatalf("read changelog fixture: %v", err)
	}

	tests := []struct {
		name                      string
		gitLog                    []byte
		wantChangelog             []byte
		wantNotesTrailingNewlines int
	}{
		{
			name:                      "hostile commit subjects",
			gitLog:                    fixture,
			wantChangelog:             fixture,
			wantNotesTrailingNewlines: 1,
		},
		{
			name:                      "empty changelog",
			gitLog:                    []byte{},
			wantChangelog:             []byte("\n"),
			wantNotesTrailingNewlines: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workDir := t.TempDir()
			runnerTemp := filepath.Join(workDir, "runner-temp")
			binDir := filepath.Join(workDir, "bin")
			if err := os.MkdirAll(runnerTemp, 0o755); err != nil {
				t.Fatalf("create runner temp: %v", err)
			}
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatalf("create fake bin: %v", err)
			}

			fixturePath := filepath.Join(workDir, "git-log.txt")
			if err := os.WriteFile(fixturePath, tt.gitLog, 0o600); err != nil {
				t.Fatalf("write git log fixture: %v", err)
			}
			writeFakeGit(t, binDir)

			markerPath := filepath.Join(workDir, "injection-marker")
			githubOutput := filepath.Join(workDir, "github-output")
			env := []string{
				"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
				"RUNNER_TEMP=" + runnerTemp,
				"CHANGELOG_FIXTURE=" + fixturePath,
				"INJECTION_MARKER=" + markerPath,
				"GITHUB_REPOSITORY_OWNER=Voluzi",
				"GITHUB_OUTPUT=" + githubOutput,
				"IMG=ghcr.io/voluzi/cosmopilot",
				"VER=3.2.1",
				"TAG=v3.2.1",
				"MANIFEST_DIGEST=sha256:manifest",
				"AMD64_DIGEST=sha256:amd64",
				"ARM64_DIGEST=sha256:arm64",
			}

			runBash(t, workDir, generate.Run, env)

			changelogPath := filepath.Join(runnerTemp, "cosmopilot-changelog.txt")
			gotChangelog, err := os.ReadFile(changelogPath)
			if err != nil {
				t.Fatalf("read generated changelog: %v", err)
			}
			if !reflect.DeepEqual(gotChangelog, tt.wantChangelog) {
				t.Fatalf("generated changelog differs byte-for-byte\nwant: %q\n got: %q", tt.wantChangelog, gotChangelog)
			}
			if got := trailingNewlines(gotChangelog); got != 1 {
				t.Fatalf("generated changelog has %d trailing newlines, want 1", got)
			}

			runBash(t, workDir, compose.Run, env)

			if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
				t.Fatalf("commit-controlled changelog executed shell code; marker stat error: %v", err)
			}

			gotNotes, err := os.ReadFile(filepath.Join(workDir, "RELEASE_NOTES.md"))
			if err != nil {
				t.Fatalf("read release notes: %v", err)
			}
			wantNotes := append([]byte(expectedReleaseNotesHeader()), tt.wantChangelog...)
			if !reflect.DeepEqual(gotNotes, wantNotes) {
				t.Fatalf("release notes differ byte-for-byte\nwant: %q\n got: %q", wantNotes, gotNotes)
			}
			if got := trailingNewlines(gotNotes); got != tt.wantNotesTrailingNewlines {
				t.Fatalf("release notes have %d trailing newlines, want %d", got, tt.wantNotesTrailingNewlines)
			}

			gotOutput, err := os.ReadFile(githubOutput)
			if err != nil {
				t.Fatalf("read GitHub output: %v", err)
			}
			if want := "body_path=RELEASE_NOTES.md\n"; string(gotOutput) != want {
				t.Fatalf("GitHub output = %q, want %q", gotOutput, want)
			}
		})
	}
}

func TestReleaseWorkflowUsesLeastPrivilegePermissions(t *testing.T) {
	t.Parallel()

	wf := loadReleaseWorkflow(t)
	want := map[string]map[string]string{
		"version": nil,
		"build": {
			"contents": "read",
			"packages": "write",
		},
		"merge": {
			"packages": "write",
		},
		"release": {
			"contents": "write",
		},
	}

	if wf.Permissions == nil || len(wf.Permissions) != 0 {
		t.Fatalf("workflow permissions = %v, want explicit empty map", wf.Permissions)
	}
	for jobName, wantPermissions := range want {
		if got := wf.Jobs[jobName].Permissions; !reflect.DeepEqual(got, wantPermissions) {
			t.Errorf("job %q permissions = %v, want %v", jobName, got, wantPermissions)
		}
	}
}

func TestReleaseCheckoutsDoNotPersistCredentials(t *testing.T) {
	t.Parallel()

	wf := loadReleaseWorkflow(t)
	checkouts := []struct {
		job  string
		step string
	}{
		{job: "build", step: "Checkout"},
		{job: "release", step: "Checkout repository"},
	}

	for _, checkout := range checkouts {
		step := findStep(t, wf.Jobs[checkout.job].Steps, checkout.step)
		if got, ok := step.With["persist-credentials"].(bool); !ok || got {
			t.Errorf("%s checkout persist-credentials = %v, want false", checkout.job, step.With["persist-credentials"])
		}
	}
}

func TestReleaseJobDoesNotLogInToContainerRegistry(t *testing.T) {
	t.Parallel()

	wf := loadReleaseWorkflow(t)
	for _, step := range wf.Jobs["release"].Steps {
		if strings.HasPrefix(step.Uses, "docker/login-action@") {
			t.Fatalf("release job unexpectedly logs in to container registry with %q", step.Uses)
		}
	}
}

func loadReleaseWorkflow(t *testing.T) workflow {
	t.Helper()

	path := filepath.Join("..", "..", ".github", "workflows", "release.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}

	var wf workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}

	return wf
}

func findStep(t *testing.T, steps []workflowStep, name string) workflowStep {
	t.Helper()

	for _, step := range steps {
		if step.Name == name {
			return step
		}
	}

	t.Fatalf("step %q not found", name)
	return workflowStep{}
}

func writeFakeGit(t *testing.T, binDir string) {
	t.Helper()

	path := filepath.Join(binDir, "git")
	script := `#!/usr/bin/env bash
set -euo pipefail

case "${1:-}" in
  tag)
    [[ "$#" -eq 2 && "$2" == "--sort=-creatordate" ]]
    printf '%s\n' v2.0.0 node-v9.9.9 v1.9.0 v1.8.0
    ;;
  log)
    [[ "$#" -eq 3 ]]
    [[ "$2" == "v1.9.0..HEAD" ]]
    [[ "$3" == "--pretty=format:- %h %s" ]]
    changelog=$(cat "$CHANGELOG_FIXTURE")
    printf '%s' "$changelog"
    ;;
  *)
    printf 'unexpected git invocation: %s\n' "$*" >&2
    exit 64
    ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
}

func runBash(t *testing.T, workDir, script string, env []string) {
	t.Helper()

	cmd := exec.Command("bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", script)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), env...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run workflow script: %v\n%s", err, output)
	}
}

func trailingNewlines(data []byte) int {
	count := 0
	for i := len(data) - 1; i >= 0 && data[i] == '\n'; i-- {
		count++
	}
	return count
}

func expectedReleaseNotesHeader() string {
	return fmt.Sprintf(`## Cosmopilot v3.2.1

**Container images**
- Multi-arch manifest: %s
- linux/amd64: %s
- linux/arm64: %s

**Install / upgrade with Helm (from OCI)**
%s
helm install cosmopilot oci://ghcr.io/voluzi/helm/cosmopilot
%s

**Changelog**
`, "`ghcr.io/voluzi/cosmopilot:3.2.1@sha256:manifest`", "`ghcr.io/voluzi/cosmopilot:3.2.1-amd64@sha256:amd64`", "`ghcr.io/voluzi/cosmopilot:3.2.1-arm64@sha256:arm64`", "```bash", "```")
}
