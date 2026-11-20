package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestImageBuildPublication(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := exec.Command("git", "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	write := func(name, contents string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "private-local\n", 0600)
	write("private-local", "synthetic omitted file", 0600)
	pin := "sha256:" + strings.Repeat("a", 64)
	write("Dockerfile", "ARG MMC_REGISTRY\nFROM ${MMC_REGISTRY}/runner@"+pin+"\n", 0600)
	manifestBody := []byte(`{"schemaVersion":2}`)
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifestBody))
	var conflict atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tag := strings.TrimPrefix(r.URL.Path, "/v2/ci/example/manifests/")
		if tag == r.URL.Path || r.Method != "GET" {
			http.NotFound(w, r)
			return
		}
		if conflict.Load() {
			_, _ = w.Write([]byte("different manifest"))
			return
		}
		published, err := os.ReadFile(filepath.Join(dir, ".git", "pushed"))
		if err != nil || (tag != string(published) && tag != digest) {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(manifestBody)
	}))
	defer srv.Close()
	old := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = old })
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".config", "mmc")
	if err := os.MkdirAll(cfgDir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`{"registry":%q,"cap":{"cpus":1,"memory":"1g"}}`, strings.TrimPrefix(srv.URL, "https://")+"/ci")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	tools := t.TempDir()
	script := `#!/bin/sh
set -eu
echo "$1 ${2-}" >> "$TEST_LOG"
case "$1 ${2-}" in
 "build --progress") for arg do ctx=$arg; done
    test -f "$ctx/Dockerfile" && test ! -e "$ctx/.git" && test ! -e "$ctx/private-local" || exit 1 ;;
 "image inspect") printf '[{"configuration":{"descriptor":{"digest":"%s"}},"variants":[{"config":{"architecture":"arm64","os":"linux","config":{"Labels":{"mmc.runner.version":"2.3.4"}}}}]}]' "$TEST_DIGEST" ;;
 "image push") test "${FAIL_PUSH-}" != yes; printf %s "${3##*:}" > "$TEST_PUSHED" ;;
 "image pull"|"builder stop"|"image delete"|"image tag") ;;
 *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(tools, "container"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+":"+os.Getenv("PATH"))
	log := filepath.Join(dir, ".git", "calls")
	t.Setenv("TEST_LOG", log)
	t.Setenv("TEST_DIGEST", digest)
	t.Setenv("TEST_PUSHED", filepath.Join(dir, ".git", "pushed"))
	invoke := func(wantErr string) {
		t.Helper()
		var out bytes.Buffer
		err := run(t.Context(), []string{"image", "build", "-name", "example"}, &out)
		if wantErr == "" {
			if err != nil || out.String() != "example@"+digest+"\n" {
				t.Fatalf("unexpected result %q: %v", out.String(), err)
			}
			return
		}
		if err == nil || !strings.Contains(err.Error(), wantErr) || out.Len() != 0 {
			t.Fatalf("expected %q; got %q, %v", wantErr, out.String(), err)
		}
	}
	t.Setenv("FAIL_PUSH", "yes")
	invoke("could not push")
	t.Setenv("FAIL_PUSH", "")
	invoke("")
	invoke("")
	conflict.Store(true)
	invoke("different image")
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for cmd, n := range map[string]int{"builder stop": 4, "image delete": 4, "image push": 2, "image pull": 2} {
		if strings.Count(string(calls), cmd+"\n") != n {
			t.Fatalf("unexpected %s count", cmd)
		}
	}
}

func TestParentDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	got, err := parentDigest([]byte("ARG BASE=ubuntu@" + digest + "\nFROM --platform=linux/arm64 ${BASE} AS build\nFROM build\n"))
	if err != nil || got != digest {
		t.Fatal("could not resolve pinned parent stage")
	}
	if _, err = parentDigest([]byte("FROM ubuntu:latest\n")); err == nil {
		t.Fatal("accepted a mutable parent")
	}
}

func TestContextFingerprint(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", dir)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	dockerfile := filepath.Join(dir, "Dockerfile")
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	from := func(hex string) string { return "FROM runner@sha256:" + strings.Repeat(hex, 64) + "\n" }
	write(dockerfile, from("a"))
	tool := filepath.Join(dir, "tool")
	write(tool, "synthetic tool")
	sum := func() [32]byte {
		t.Helper()
		h, err := stageContext(t.Context(), dir, t.TempDir(), false)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	first := sum()
	if sum() != first {
		t.Fatal("unchanged context produced a different fingerprint")
	}
	if err := os.Chmod(tool, 0700); err != nil {
		t.Fatal(err)
	}
	executable := sum()
	if executable == first {
		t.Fatal("executable bit missing from fingerprint")
	}
	write(dockerfile, from("b"))
	if sum() == executable {
		t.Fatal("parent change missing from fingerprint")
	}
}
