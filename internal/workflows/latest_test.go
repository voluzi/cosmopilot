package workflows

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsNewestReleaseOnlyMovesLatestForTheNewestStableRelease(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "--allow-empty", "-m", "init")
	for _, tag := range []string{
		"v3.1.0", "v3.2.0", "v3.2.1", "v3.10.0-rc.1", "v2.9.9",
		"node-utils/v2.9.3", "node-utils/v3.0.0-beta.2", "node-utils/v3.0.0", "node-utils/v10.0.0-beta.1",
		"node-tools/v1.4.3", "node-tools/v1.10.0", "chart/v9.9.9", "vault-renewer/v1.0.1",
	} {
		git(t, repo, "tag", tag)
	}

	tests := []struct {
		prefix  string
		version string
		want    string
	}{
		{prefix: "", version: "3.2.1", want: "true"},
		{prefix: "", version: "3.2.0", want: "false"},           // older release re-run
		{prefix: "", version: "2.9.9", want: "false"},           // backport to an older line
		{prefix: "", version: "3.10.0-rc.1", want: "false"},     // prerelease, even when highest
		{prefix: "node-utils/", version: "3.0.0", want: "true"}, // component tags are scoped by prefix
		{prefix: "node-utils/", version: "3.0.0-beta.2", want: "false"},
		{prefix: "node-utils/", version: "2.9.3", want: "false"},
		{prefix: "node-tools/", version: "1.10.0", want: "true"}, // numeric, not lexical, order
		{prefix: "node-tools/", version: "1.4.3", want: "false"},
		{prefix: "vault-renewer/", version: "1.0.1", want: "true"},
	}

	script, err := filepath.Abs(filepath.Join("..", "..", ".github", "scripts", "is-newest-release.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		cmd := exec.Command("bash", script, tt.prefix, tt.version)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("is-newest-release.sh %q %q: %v\n%s", tt.prefix, tt.version, err, out)
		}
		if got := strings.TrimSpace(string(out)); got != tt.want {
			t.Errorf("is-newest-release.sh %q %q = %s, want %s", tt.prefix, tt.version, got, tt.want)
		}
	}
}

func TestReleaseWorkflowsGateLatestOnTheNewestRelease(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"node-utils", "node-tools", "data-exporter", "vault-renewer"} {
		data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name+"-docker.yaml"))
		if err != nil {
			t.Fatalf("read %s workflow: %v", name, err)
		}
		content := string(data)
		for _, want := range []string{
			"type=raw,value=latest-${{ env.ARCH }},enable=${{ needs.version.outputs.latest }}",
			"type=raw,value=latest,enable=${{ needs.version.outputs.latest }}",
			".github/scripts/is-newest-release.sh",
		} {
			if !strings.Contains(content, want) {
				t.Errorf("%s workflow is missing %q", name, want)
			}
		}
		for _, line := range strings.Split(content, "\n") {
			if trimmed := strings.TrimSpace(line); trimmed == "latest" || trimmed == "latest-${{ env.ARCH }}" {
				t.Errorf("%s workflow still publishes an unconditional %q tag", name, trimmed)
			}
		}
	}

	wf := loadReleaseWorkflow(t)
	release := findStep(t, wf.Jobs["release"].Steps, "Create GitHub Release")
	if got := release.With["make_latest"]; got != "${{ needs.version.outputs.latest }}" {
		t.Errorf("GitHub release make_latest = %v, want the newest-release gate", got)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
