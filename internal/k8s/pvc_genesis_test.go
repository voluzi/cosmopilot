package k8s

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenesisDownloadCommandDigestValidation(t *testing.T) {
	genesis := []byte("{\"chain_id\":\"test-1\"}\n")
	digest := sha256Hex(genesis)
	uppercaseDigest := strings.ToUpper(digest)

	tests := []struct {
		name       string
		digest     *string
		wantErr    bool
		existing   bool
		wantOutput string
	}{
		{name: "correct digest", digest: &digest, wantOutput: string(genesis)},
		{name: "nil digest", wantOutput: string(genesis)},
		{name: "wrong digest preserves destination", digest: ptrToString(strings.Repeat("0", 64)), wantErr: true, existing: true},
		{name: "wrong digest does not create destination", digest: ptrToString(strings.Repeat("f", 64)), wantErr: true},
		{name: "empty digest", digest: ptrToString(""), wantErr: true, existing: true},
		{name: "uppercase digest", digest: &uppercaseDigest, wantErr: true, existing: true},
		{name: "malformed digest", digest: ptrToString("not-a-sha256"), wantErr: true, existing: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			destination := filepath.Join(dir, "genesis.json")
			if tt.existing {
				require.NoError(t, os.WriteFile(destination, []byte("existing"), 0o600))
			}

			output, err := runGenesisDownloadCommand(t, "https://example.invalid/genesis.json", destination, tt.digest, genesis, "")
			if tt.wantErr {
				require.Error(t, err, output)
				assert.Contains(t, output, "genesis SHA256 mismatch")
				if tt.existing {
					assertFileContents(t, destination, "existing")
				} else {
					assert.NoFileExists(t, destination)
				}
				return
			}

			require.NoError(t, err, output)
			assertFileContents(t, destination, tt.wantOutput)
		})
	}
}

func TestGenesisDownloadCommandHashesDecompressedBytes(t *testing.T) {
	genesis := []byte("{\"chain_id\":\"compressed-1\"}\n")
	digest := sha256Hex(genesis)

	tests := []struct {
		name       string
		extension  string
		compressed func(*testing.T, []byte) []byte
	}{
		{name: "gzip", extension: ".gz", compressed: gzipBytes},
		{name: "zstd", extension: ".zst", compressed: zstdBytes},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compressed := tt.compressed(t, genesis)
			dir := t.TempDir()
			destination := filepath.Join(dir, "genesis.json")

			output, err := runGenesisDownloadCommand(t, "https://example.invalid/genesis.json"+tt.extension, destination, &digest, compressed, "")
			require.NoError(t, err, output)
			assertFileContents(t, destination, string(genesis))

			compressedDigest := sha256Hex(compressed)
			require.NoError(t, os.WriteFile(destination, []byte("existing"), 0o600))
			output, err = runGenesisDownloadCommand(t, "https://example.invalid/genesis.json"+tt.extension, destination, &compressedDigest, compressed, "")
			require.Error(t, err, output)
			assertFileContents(t, destination, "existing")

			output, err = runGenesisDownloadCommand(t, "https://example.invalid/genesis.json"+tt.extension, destination, nil, compressed[:len(compressed)/2], "")
			require.Error(t, err, output)
			assertFileContents(t, destination, "existing")
		})
	}
}

func TestGenesisDownloadCommandRejectsFailedTransferBeforePublication(t *testing.T) {
	genesis := []byte("{\"chain_id\":\"test-1\"}\n")

	for _, tt := range []struct {
		mode     string
		existing bool
	}{
		{mode: "partial-fail"},
		{mode: "complete-fail", existing: true},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			dir := t.TempDir()
			destination := filepath.Join(dir, "genesis.json")
			if tt.existing {
				require.NoError(t, os.WriteFile(destination, []byte("existing"), 0o600))
			}

			output, err := runGenesisDownloadCommand(t, "https://example.invalid/genesis.json.gz", destination, nil, gzipBytes(t, genesis), tt.mode)
			require.Error(t, err, output)
			if tt.existing {
				assertFileContents(t, destination, "existing")
			} else {
				assert.NoFileExists(t, destination)
			}
		})
	}
}

func TestGenesisDownloadCommandTreatsInputsAsData(t *testing.T) {
	dir := t.TempDir()
	destinationDir := filepath.Join(dir, "destination with spaces")
	require.NoError(t, os.Mkdir(destinationDir, 0o700))
	destination := filepath.Join(destinationDir, "genesis 'quoted'.json")
	sentinel := filepath.Join(dir, "executed")
	malformedDigest := fmt.Sprintf("bad'; touch %s; echo '", sentinel)
	url := "https://example.invalid/genesis'; touch " + sentinel + "; echo '.json"

	output, err := runGenesisDownloadCommand(t, url, destination, &malformedDigest, []byte("genesis\n"), "")
	require.Error(t, err, output)
	assert.NoFileExists(t, sentinel)
	assert.NoFileExists(t, destination)
}

func TestGenesisDownloadCommandInterruptDoesNotPublish(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "genesis.json")
	require.NoError(t, os.WriteFile(destination, []byte("existing"), 0o600))
	readyFile := filepath.Join(dir, "ready")

	args := buildGenesisDownloadCommand("https://example.invalid/genesis.json", destination, nil)
	cmd := exec.Command("/bin/sh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = genesisCommandEnvironment(t, []byte("partial"), "block", readyFile)
	require.NoError(t, cmd.Start())
	waitForGenesisCommandReady(t, readyFile, cmd)

	require.NoError(t, syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM))
	require.Error(t, cmd.Wait())
	assertFileContents(t, destination, "existing")
	matches, err := filepath.Glob(filepath.Join(dir, ".genesis.json.download.*"))
	require.NoError(t, err)
	assert.Empty(t, matches)
}

func TestGenesisDownloadCommandRetryRemovesSIGKILLStaging(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "genesis.json")
	require.NoError(t, os.WriteFile(destination, []byte("existing"), 0o600))
	readyFile := filepath.Join(dir, "ready")

	args := buildGenesisDownloadCommand("https://example.invalid/genesis.json", destination, nil)
	cmd := exec.Command("/bin/sh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = genesisCommandEnvironment(t, bytes.Repeat([]byte("partial"), 16*1024), "block", readyFile)
	require.NoError(t, cmd.Start())
	waitForGenesisCommandReady(t, readyFile, cmd)

	require.NoError(t, syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL))
	require.Error(t, cmd.Wait())
	assertFileContents(t, destination, "existing")
	abandoned, err := filepath.Glob(filepath.Join(dir, ".genesis.json.download.*"))
	require.NoError(t, err)
	require.NotEmpty(t, abandoned)

	output, err := runGenesisDownloadCommand(t, "https://example.invalid/genesis.json", destination, nil, []byte("replacement"), "")
	require.NoError(t, err, output)
	assertFileContents(t, destination, "replacement")
	abandoned, err = filepath.Glob(filepath.Join(dir, ".genesis.json.download.*"))
	require.NoError(t, err)
	assert.Empty(t, abandoned)
}

func TestGenesisDownloadCommandReclaimsOnlyDestinationStagePrefix(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "genesis.json")
	staleStage := filepath.Join(dir, ".genesis.json.download.abandoned")
	require.NoError(t, os.Mkdir(staleStage, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(staleStage, "partial"), []byte("partial"), 0o600))
	sibling := filepath.Join(dir, ".genesis.json.download-backup")
	require.NoError(t, os.Mkdir(sibling, 0o700))

	output, err := runGenesisDownloadCommand(t, "https://example.invalid/genesis.json", destination, nil, []byte("replacement"), "")
	require.NoError(t, err, output)
	assert.NoDirExists(t, staleStage)
	assert.DirExists(t, sibling)
}

func TestGenesisDownloadCommandOverlappingAttemptCleanupIsIsolated(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "genesis.json")
	require.NoError(t, os.WriteFile(destination, []byte("existing"), 0o600))

	oldReady := filepath.Join(dir, "old-ready")
	oldCmd := startBlockingGenesisCommand(t, destination, oldReady, []byte("old partial"))
	waitForGenesisCommandReady(t, oldReady, oldCmd)

	retryReady := filepath.Join(dir, "retry-ready")
	retryCmd := startBlockingGenesisCommand(t, destination, retryReady, []byte("retry partial"))
	waitForGenesisCommandReady(t, retryReady, retryCmd)
	retryStages, err := filepath.Glob(filepath.Join(dir, ".genesis.json.download.*"))
	require.NoError(t, err)
	require.Len(t, retryStages, 1)
	retryStage := retryStages[0]

	require.NoError(t, syscall.Kill(-oldCmd.Process.Pid, syscall.SIGTERM))
	require.Error(t, oldCmd.Wait())
	assert.DirExists(t, retryStage)
	assertFileContents(t, destination, "existing")

	require.NoError(t, syscall.Kill(-retryCmd.Process.Pid, syscall.SIGTERM))
	require.Error(t, retryCmd.Wait())
	assert.NoDirExists(t, retryStage)
}

func TestGenesisDownloadCommandRefusesDirectoryDestination(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "genesis.json")
	require.NoError(t, os.Mkdir(destination, 0o700))

	output, err := runGenesisDownloadCommand(t, "https://example.invalid/genesis.json", destination, nil, []byte("replacement"), "")
	require.Error(t, err, output)
	assert.DirExists(t, destination)
	assert.NoFileExists(t, filepath.Join(destination, "genesis"))
}

func TestGenesisDownloadCommandRefusesSymlinkToDirectoryDestination(t *testing.T) {
	dir := t.TempDir()
	targetDir := filepath.Join(dir, "target")
	require.NoError(t, os.Mkdir(targetDir, 0o700))
	destination := filepath.Join(dir, "genesis.json")
	require.NoError(t, os.Symlink(targetDir, destination))

	output, err := runGenesisDownloadCommand(t, "https://example.invalid/genesis.json", destination, nil, []byte("replacement"), "")
	require.Error(t, err, output)
	info, statErr := os.Lstat(destination)
	require.NoError(t, statErr)
	assert.NotZero(t, info.Mode()&os.ModeSymlink)
	assert.NoFileExists(t, filepath.Join(targetDir, "genesis"))
}

func runGenesisDownloadCommand(t *testing.T, url, destination string, digest *string, source []byte, mode string) (string, error) {
	t.Helper()
	args := buildGenesisDownloadCommand(url, destination, digest)
	cmd := exec.Command("/bin/sh", args...)
	cmd.Env = genesisCommandEnvironment(t, source, mode, "")
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func startBlockingGenesisCommand(t *testing.T, destination, readyFile string, source []byte) *exec.Cmd {
	t.Helper()
	args := buildGenesisDownloadCommand("https://example.invalid/genesis.json", destination, nil)
	cmd := exec.Command("/bin/sh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = genesisCommandEnvironment(t, source, "block", readyFile)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd
}

func genesisCommandEnvironment(t *testing.T, source []byte, mode, readyFile string) []string {
	t.Helper()
	toolsDir := t.TempDir()
	for _, tool := range []string{"wget", "gunzip", "zstd", "sha256sum", "mv"} {
		wrapper := "#!/bin/sh\nGENESIS_TEST_TOOL=" + tool + " exec \"$GENESIS_TEST_BINARY\" -test.run=TestGenesisCommandTool -- \"$@\"\n"
		require.NoError(t, os.WriteFile(filepath.Join(toolsDir, tool), []byte(wrapper), 0o700))
	}
	sourcePath := filepath.Join(t.TempDir(), "source")
	require.NoError(t, os.WriteFile(sourcePath, source, 0o600))
	return append(os.Environ(),
		"PATH="+toolsDir+":"+os.Getenv("PATH"),
		"GENESIS_TEST_BINARY="+os.Args[0],
		"GENESIS_TEST_SOURCE="+sourcePath,
		"GENESIS_TEST_WGET_MODE="+mode,
		"GENESIS_TEST_READY_FILE="+readyFile,
	)
}

func waitForGenesisCommandReady(t *testing.T, readyFile string, cmd *exec.Cmd) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			t.Fatal("timed out waiting for staged partial download")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestGenesisCommandTool(t *testing.T) {
	tool := os.Getenv("GENESIS_TEST_TOOL")
	if tool == "" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}

	var err error
	switch tool {
	case "wget":
		err = genesisTestWget(args)
	case "gunzip":
		err = genesisTestGunzip(args)
	case "zstd":
		err = genesisTestZstd(args)
	case "sha256sum":
		err = genesisTestSHA256Sum(args)
	case "mv":
		err = genesisTestMV(args)
	default:
		err = fmt.Errorf("unknown helper tool %q", tool)
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func genesisTestWget(args []string) error {
	if len(args) != 4 || args[0] != "-q" || args[1] != "-O" {
		return fmt.Errorf("unexpected wget args: %q", args)
	}
	source, err := os.ReadFile(os.Getenv("GENESIS_TEST_SOURCE"))
	if err != nil {
		return err
	}
	mode := os.Getenv("GENESIS_TEST_WGET_MODE")
	if mode == "partial-fail" || mode == "block" {
		source = source[:max(1, len(source)/2)]
	}
	if err := os.WriteFile(args[2], source, 0o600); err != nil {
		return err
	}
	switch mode {
	case "partial-fail", "complete-fail":
		return fmt.Errorf("controlled transfer failure")
	case "block":
		if err := os.WriteFile(os.Getenv("GENESIS_TEST_READY_FILE"), []byte("ready"), 0o600); err != nil {
			return err
		}
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		return fmt.Errorf("interrupted")
	default:
		return nil
	}
}

func genesisTestGunzip(args []string) error {
	if len(args) != 2 || args[0] != "-c" {
		return fmt.Errorf("unexpected gunzip args: %q", args)
	}
	f, err := os.Open(args[1])
	if err != nil {
		return err
	}
	defer f.Close()
	r, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(os.Stdout, r)
	return err
}

func genesisTestZstd(args []string) error {
	if len(args) != 3 || args[0] != "-d" || args[1] != "-c" {
		return fmt.Errorf("unexpected zstd args: %q", args)
	}
	f, err := os.Open(args[2])
	if err != nil {
		return err
	}
	defer f.Close()
	r, err := zstd.NewReader(f)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(os.Stdout, r)
	return err
}

func genesisTestSHA256Sum(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("unexpected sha256sum args: %q", args)
	}
	content, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(os.Stdout, "%s  %s\n", sha256Hex(content), args[0])
	return err
}

func genesisTestMV(args []string) error {
	noTargetDirectory := false
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if strings.Contains(args[0], "T") {
			noTargetDirectory = true
		}
		args = args[1:]
	}
	if len(args) != 2 {
		return fmt.Errorf("unexpected mv args: %q", args)
	}
	source, destination := args[0], args[1]
	if info, err := os.Lstat(destination); err == nil {
		if noTargetDirectory && (info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return fmt.Errorf("refusing directory destination %q", destination)
		}
		if !noTargetDirectory {
			if targetInfo, statErr := os.Stat(destination); statErr == nil && targetInfo.IsDir() {
				destination = filepath.Join(destination, filepath.Base(source))
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(source, destination)
}

func gzipBytes(t *testing.T, content []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	_, err := w.Write(content)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return out.Bytes()
}

func zstdBytes(t *testing.T, content []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := zstd.NewWriter(&out)
	require.NoError(t, err)
	_, err = w.Write(content)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return out.Bytes()
}

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func ptrToString(value string) *string {
	return &value
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, string(content))
}
