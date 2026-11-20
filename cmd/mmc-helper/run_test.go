package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Re-exec the CLI in a separate process, using a private application support
// directory. The shipped command always derives this directory from the user.
func TestRunChild(t *testing.T) {
	if os.Getenv("MMC_TEST_CHILD") != "1" {
		return
	}
	os.Exit(runHelperAt([]string{"-config", os.Getenv("MMC_TEST_CONFIG")}, os.Getenv("MMC_TEST_SUPPORT")))
}

func TestRunRejectsDuplicateAndReleasesLock(t *testing.T) {
	temp := t.TempDir()
	support := filepath.Join(temp, "support")
	path := filepath.Join(temp, "config.json")
	write := func(path, content string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(path, `{"registry":"registry.invalid","cap":{"cpus":1,"memory":"1g"},"repos":{},
"isolation":{"probeBinary":"probe","hostLAN":"192.0.2.1","tailnetResolver":"192.0.2.2","image":"example.invalid/probe@sha256:`+strings.Repeat("0", 64)+`"}}`, 0600)
	// Hold the first inventory read open, then return an invalid response.
	// No probe or GitHub session is reachable from this fixture.
	write(filepath.Join(temp, "container"), `#!/bin/sh
set -eu
[ "$*" = 'list --all --format json' ] || exit 2
printf 'inventory\n' >> "$MMC_TEST_CALLS"
while [ ! -f "$MMC_TEST_RELEASE" ]; do sleep 0.01; done
printf 'null\n'
`, 0700)
	t.Setenv("MMC_TEST_CHILD", "1")
	t.Setenv("MMC_TEST_SUPPORT", support)
	t.Setenv("MMC_TEST_CONFIG", path)
	t.Setenv("MMC_TEST_CALLS", filepath.Join(temp, "calls"))
	t.Setenv("MMC_TEST_RELEASE", filepath.Join(temp, "release"))
	t.Setenv("MMC_GITHUB_APP_CLIENT_ID", "synthetic")
	t.Setenv("MMC_GITHUB_APP_INSTALLATION_ID", "1")
	t.Setenv("PATH", temp+":/usr/bin:/bin")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fakePEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	proc := func() *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunChild$")
		cmd.Stdin = bytes.NewReader(fakePEM)
		return cmd
	}
	exitCode := func(err error, want int) {
		t.Helper()
		var status *exec.ExitError
		if !errors.As(err, &status) || status.ExitCode() != want {
			t.Fatalf("unexpected exit %v, want %d", err, want)
		}
	}
	first := proc()
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = first.Process.Kill()
		_ = os.WriteFile(os.Getenv("MMC_TEST_RELEASE"), nil, 0600)
	}()
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err := os.Stat(os.Getenv("MMC_TEST_CALLS")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first helper did not reach inventory")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Setenv("MMC_TEST_CONFIG", filepath.Join(temp, "other.json"))
	exitCode(proc().Run(), 3)
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	exitCode(first.Wait(), -1)
	write(os.Getenv("MMC_TEST_RELEASE"), "", 0600)
	t.Setenv("MMC_TEST_CONFIG", path)
	exitCode(proc().Run(), 1)
	calls, err := os.ReadFile(os.Getenv("MMC_TEST_CALLS"))
	if err != nil || string(calls) != "inventory\ninventory\n" {
		t.Fatal("expected inventory only for first helper and restart")
	}
}
