package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureContainerRuntime(t *testing.T) {
	dir := t.TempDir()
	const fake = `#!/bin/sh
echo "$*" >> "$CALLS"
case "$*" in
  "system status") exit "$STATUS" ;;
  "system start --disable-kernel-install --timeout 30") exit "$START" ;;
  *) exit 99 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "container"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	const status = "system status\n"
	const start = "system start --disable-kernel-install --timeout 30\n"
	for _, tc := range []struct {
		name, statusCode, startCode, calls string
		wantErr                            bool
	}{
		{"already running", "0", "1", status, false},
		{"cold login", "1", "0", status + start, false},
		{"unavailable", "1", "1", status + start, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := filepath.Join(t.TempDir(), "calls")
			t.Setenv("CALLS", calls)
			t.Setenv("STATUS", tc.statusCode)
			t.Setenv("START", tc.startCode)
			if err := ensureContainerRuntime(t.Context()); (err != nil) != tc.wantErr {
				t.Fatalf("unexpected startup result: %v", err)
			}
			data, err := os.ReadFile(calls)
			if err != nil || string(data) != tc.calls {
				t.Fatalf("runtime calls %q, error %v", data, err)
			}
		})
	}
}
