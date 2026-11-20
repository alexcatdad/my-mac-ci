package launcher

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexcatdad/my-mac-ci/internal/config"
	"github.com/alexcatdad/my-mac-ci/internal/scheduler"
)

const (
	testName  = "mmc-synthetic"
	synthetic = "synthetic-credential-string"
)

var (
	testImage  = "registry.invalid/ci/synthetic@sha256:" + strings.Repeat("a", 64)
	testKey    = scheduler.Key{Repo: "example/project", Label: "linux"}
	allocation = config.Resources{CPUs: 2, Memory: 200<<20 + 123}
)

const fakeContainer = `#!/bin/sh
set -eu
cd "$MMC_TEST_DIR"
case "$1" in
image)
    test "$2" = inspect
    test ! -f image-unavailable
    ;;
run)
    printf '%s\n' "$@" > args
    cat > stdin
    if env | grep -qF 'synthetic-credential-string'; then touch leaked-env; fi
    if test -f hold; then exec sleep 60; fi
    if test -f failed; then
        cat stdin
        cat stdin >&2
        exit 1
    fi
    ;;
list)
    test "$*" = 'list --all --format json'
    touch listed
    if test -f inventory; then cat inventory; exit 0; fi
    if test -f present; then
        label=$(sed -n 's/^com.alexcatdad.mmc.runner=//p' args)
        if test -f foreign; then label=foreign; fi
        printf '[{"id":"mmc-synthetic","configuration":{"id":"mmc-synthetic","labels":{"com.alexcatdad.mmc.runner":"%s"}},"status":{"state":"running","networks":[]}}]\n' "$label"
    else
        echo '[]'
    fi
    ;;
rm)
    test "$*" = 'rm -f mmc-synthetic'
    touch removed
    ;;
*) exit 3 ;;
esac
`

func fake(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "container"), []byte(fakeContainer), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MMC_TEST_DIR", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func put(t *testing.T, dir, filename, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func waitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("fake runtime did not reach expected state")
}

func waitEnd(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("launcher did not confirm exit")
		return nil
	}
}

func allow(start func() error) error { return start() }

func TestRunContract(t *testing.T) {
	dir := fake(t)
	done, err := Start(t.Context(), testName, testKey, testImage, allocation, synthetic, allow)
	if err != nil || waitEnd(t, done) != nil {
		t.Fatal("expected successful runner lifecycle")
	}
	data, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Fields(string(data))
	if len(args) < 8 || !strings.HasPrefix(args[6], ownershipLabel+"=") {
		t.Fatal("missing ownership metadata")
	}
	_, digest, _ := strings.Cut(testImage, "@")
	want := []string{"run", "--rm", "-i", "--name", testName, "--label", args[6], "--label", repoLabel + "=" + testKey.Repo, "--label", runnerLabel + "=" + testKey.Label, "--label", digestLabel + "=" + digest, "--cpus", "2",
		"--memory", "209715323b", "--network", "mmc", "--dns", "1.1.1.1", "--kernel-arg", "ipv6.disable=1",
		"--tmpfs", "/run/mmc", "--entrypoint", "/usr/local/bin/mmc-runner", testImage}
	if !slices.Equal(args, want) {
		t.Fatal("unexpected runner CLI arguments")
	}
	input, err := os.ReadFile(filepath.Join(dir, "stdin"))
	if err != nil || string(input) != synthetic+"\n" {
		t.Fatal("JIT data did not arrive on stdin")
	}
	if _, err := os.Stat(filepath.Join(dir, "leaked-env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("JIT data reached the CLI environment")
	}
}

func TestLaunchGuard(t *testing.T) {
	dir := fake(t)
	deny := func(func() error) error { return context.Canceled }
	done, err := Start(t.Context(), testName, testKey, testImage, allocation, synthetic, deny)
	if done != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled authorization should prevent a spawn")
	}
	put(t, dir, "image-unavailable", "")
	done, err = Start(t.Context(), testName, testKey, testImage, allocation, synthetic, allow)
	if done != nil || err == nil {
		t.Fatal("missing local image should prevent a spawn")
	}
	done, err = Start(t.Context(), testName, testKey, "registry.invalid/image:latest", allocation, synthetic, allow)
	if done != nil || err == nil || strings.Contains(err.Error(), "registry.invalid") {
		t.Fatal("unpinned image should produce a redacted error")
	}
	if _, err := os.Stat(filepath.Join(dir, "args")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("blocked runner started")
	}
}

func TestFailureWaitsForConclusiveAbsence(t *testing.T) {
	dir := fake(t)
	for _, name := range []string{"present", "failed"} {
		put(t, dir, name, "")
	}
	put(t, dir, "inventory", `[{"configuration":{},"status":{"state":"running"}}]`)
	done, err := Start(t.Context(), testName, testKey, testImage, allocation, synthetic, allow)
	if err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(dir, "listed"))
	select {
	case unexpected := <-done:
		t.Fatalf("released after incomplete inventory: %v", unexpected)
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.Remove(filepath.Join(dir, "inventory")); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(dir, "removed"))
	select {
	case unexpected := <-done:
		t.Fatalf("released before removal was confirmed: %v", unexpected)
	default:
	}
	if err := os.Remove(filepath.Join(dir, "present")); err != nil {
		t.Fatal(err)
	}
	if result := waitEnd(t, done); result != errRun {
		t.Fatal("runner failure must remain redacted")
	}
}

func TestRunnerFailureKeepsJITOutOfLogs(t *testing.T) {
	const fixture = "MMC_TEST_JIT_LOG_FIXTURE"
	if os.Getenv(fixture) == "1" {
		dir := fake(t)
		put(t, dir, "failed", "")
		done, err := Start(t.Context(), testName, testKey, testImage, allocation, synthetic, allow)
		if err != nil || waitEnd(t, done) != errRun {
			t.Fatal("expected a redacted runtime error")
		}
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunnerFailureKeepsJITOutOfLogs$")
	cmd.Env = append(os.Environ(), fixture+"=1")
	output, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(string(output), synthetic) {
		t.Fatal("runtime failure fixture failed or exposed synthetic JIT data")
	}
}

func TestCancelNeverRemovesForeignContainer(t *testing.T) {
	dir := fake(t)
	for _, name := range []string{"hold", "present", "foreign"} {
		put(t, dir, name, "")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done, err := Start(ctx, testName, testKey, testImage, allocation, synthetic, allow)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(dir, "stdin"))
	cancel()
	waitFile(t, filepath.Join(dir, "listed"))
	if _, err := os.Stat(filepath.Join(dir, "removed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("foreign container was removed")
	}
	if err := os.Remove(filepath.Join(dir, "present")); err != nil {
		t.Fatal(err)
	}
	if result := waitEnd(t, done); result != errRun {
		t.Fatal("expected cancelled run")
	}
}

const sampleRow = `{"id":"mmc-synthetic","configuration":{"id":"mmc-synthetic","labels":{"com.alexcatdad.mmc.runner":"nonce","com.alexcatdad.mmc.repo":"example/project","com.alexcatdad.mmc.label":"linux"},"resources":{"cpus":2,"memoryInBytes":209715323}},"status":{"state":"running"}}`

func TestInventory(t *testing.T) {
	dir := fake(t)
	foreign := `{"id":"mmc-unrelated","configuration":{"id":"mmc-unrelated","labels":{}},"status":{"state":"running"}}`
	put(t, dir, "inventory", "["+foreign+","+sampleRow+"]")
	runners, err := Inventory(t.Context())
	if err != nil || len(runners) != 1 || runners[0] != (Survivor{Name: testName, Resources: allocation, Key: testKey, owner: "nonce"}) {
		t.Fatal("expected only the owned VM and its observed allocation")
	}
	for _, bad := range []string{
		strings.Replace(sampleRow, `,"com.alexcatdad.mmc.repo":"example/project"`, "", 1),
		strings.Replace(sampleRow, `,"com.alexcatdad.mmc.label":"linux"`, "", 1),
		strings.Replace(sampleRow, `"memoryInBytes":209715323`, `"memoryInBytes":0`, 1),
		sampleRow + "," + sampleRow,
	} {
		put(t, dir, "inventory", "["+bad+"]")
		if rows, err := Inventory(t.Context()); rows != nil || err == nil || strings.Contains(err.Error(), "example") {
			t.Fatal("ambiguous inventory must fail closed with a redacted error")
		}
	}
}

func TestSurvivorMonitor(t *testing.T) {
	for _, detach := range []bool{false, true} {
		name := "stop"
		if detach {
			name = "detach"
		}
		t.Run(name, func(t *testing.T) {
			dir := fake(t)
			put(t, dir, "inventory", "["+sampleRow+"]")
			runners, err := Inventory(t.Context())
			if err != nil || len(runners) != 1 {
				t.Fatal("expected a survivor")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stop, requestStop := context.WithCancel(t.Context())
			defer requestStop()
			if detach {
				// Supervisor setup error: detach even if a stop was also queued.
				cancel()
				requestStop()
				if runners[0].Wait(ctx, stop) {
					t.Fatal("detaching cannot establish container absence")
				}
				if _, err := os.Stat(filepath.Join(dir, "removed")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("detaching must preserve a live job")
				}
				return
			}
			done := make(chan bool, 1)
			go func() { done <- runners[0].Wait(ctx, stop) }()
			requestStop()
			waitFile(t, filepath.Join(dir, "removed"))
			select {
			case <-done:
				t.Fatal("a stop request is not confirmation that the container exited")
			default:
			}
			put(t, dir, "inventory", "[]")
			select {
			case absent := <-done:
				if !absent {
					t.Fatal("expected confirmed removal")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("monitor did not observe exit")
			}
		})
	}
}
