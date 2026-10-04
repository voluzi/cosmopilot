package workflows

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func loadE2EWorkflow(t *testing.T) workflow {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "e2e.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}
	return wf
}

func TestE2EImageArtifactSuppliesShardImages(t *testing.T) {
	wf := loadE2EWorkflow(t)
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0755); err != nil {
		t.Fatal(err)
	}
	// Simulate Docker's archive transport without building images or starting a daemon.
	docker := `#!/usr/bin/env bash
set -euo pipefail
if [ "$1" = load ]; then
  cat "$3" >> "$LOADED_IMAGES"
  exit 0
fi
image=''
archive=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    -t) image="$2"; shift ;;
    --output=type=docker,dest=*) archive="${1#--output=type=docker,dest=}" ;;
  esac
  shift
done
printf '%s\n' "$image" > "$archive"
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0755); err != nil {
		t.Fatal(err)
	}
	images := map[string]string{
		"controller": "fixture/controller:1", "node-utils": "fixture/node-utils:2",
		"dataexporter": "fixture/dataexporter:3", "minio": "fixture/minio:4",
	}
	variables := map[string]string{
		"controller": "IMG", "node-utils": "NODE_UTILS_IMG", "dataexporter": "DATA_EXPORTER_IMG", "minio": "MINIO_IMG",
	}
	output := filepath.Join(dir, "outputs")
	loaded := filepath.Join(dir, "loaded")
	env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "GITHUB_OUTPUT=" + output, "LOADED_IMAGES=" + loaded}
	for name, variable := range variables {
		env = append(env, variable+"="+images[name])
	}
	steps := wf.Jobs["build-images"].Steps
	runBash(t, root, findStep(t, steps, "Resolve image tags").Run, env)
	build := strings.ReplaceAll(findStep(t, steps, "Build images").Run, "/tmp/images", filepath.Join(dir, "images"))
	runBash(t, root, build, env)
	load := strings.ReplaceAll(findStep(t, wf.Jobs["e2e"].Steps, "Load images").Run, "/tmp/images", filepath.Join(dir, "images"))
	runBash(t, root, load, env)
	loadedBytes, err := os.ReadFile(loaded)
	if err != nil {
		t.Fatal(err)
	}
	resolvedBytes, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	resolved := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(resolvedBytes)), "\n") {
		name, image, _ := strings.Cut(line, "=")
		resolved[name] = image
	}
	for name, image := range images {
		if resolved[name] != image || !strings.Contains(string(loadedBytes), image+"\n") {
			t.Errorf("%s image %q was not resolved and transported to the shard", name, image)
		}
	}
	// Resolve the cross-job references and capture the arguments handed to the test target.
	args := filepath.Join(dir, "args")
	if err := os.WriteFile(filepath.Join(bin, "make"), []byte("#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" > \"$SHARD_ARGS\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	shard := findStep(t, wf.Jobs["e2e"].Steps, "Run E2E Tests (${{ matrix.shard }})").Run
	for outputName, reference := range wf.Jobs["build-images"].Outputs {
		key := strings.TrimSuffix(strings.TrimPrefix(reference, "${{ steps.images.outputs."), " }}")
		shard = strings.ReplaceAll(shard, "${{ needs.build-images.outputs."+outputName+" }}", resolved[key])
	}
	shard = regexp.MustCompile(`\$\{\{[^}]*\}\}`).ReplaceAllString(shard, "")
	runBash(t, root, shard, append(env, "SHARD_ARGS="+args))
	arguments, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	for name, variable := range variables {
		if !strings.Contains(string(arguments), variable+"="+images[name]+"\n") {
			t.Errorf("shard did not receive %s=%s", variable, images[name])
		}
	}
}

func TestE2ERejectsUnresolvedImageTags(t *testing.T) {
	wf := loadE2EWorkflow(t)
	resolve := findStep(t, wf.Jobs["build-images"].Steps, "Resolve image tags").Run
	for _, variable := range []string{"IMG", "NODE_UTILS_IMG", "DATA_EXPORTER_IMG", "MINIO_IMG"} {
		t.Run(variable, func(t *testing.T) {
			cmd := exec.Command("bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", resolve)
			cmd.Dir = filepath.Join("..", "..")
			cmd.Env = append(os.Environ(), "IMG=fixture/controller:1", "NODE_UTILS_IMG=fixture/node-utils:2", "DATA_EXPORTER_IMG=fixture/dataexporter:3", "MINIO_IMG=fixture/minio:4", variable+"=fixture/unresolved:", "GITHUB_OUTPUT="+filepath.Join(t.TempDir(), "outputs"))
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "could not resolve an image tag") {
				t.Fatalf("unresolved %s was not rejected: %v\n%s", variable, err, out)
			}
		})
	}
}
