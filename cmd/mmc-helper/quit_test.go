package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuitBeforeCredentialsAccountsForSurvivingJob(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	t.Setenv("MMC_TEST_RUNTIME", dir)
	const runtime = `#!/bin/sh
case "$1" in
list)
	if test -f "$MMC_TEST_RUNTIME/gone"; then echo '[]'; else
	echo '[{"id":"mmc-test","configuration":{"id":"mmc-test","labels":{"com.alexcatdad.mmc.runner":"synthetic","com.alexcatdad.mmc.repo":"example/test","com.alexcatdad.mmc.label":"linux"},"resources":{"cpus":1,"memoryInBytes":1024}},"status":{"state":"running"}}]'
	fi ;;
rm) test "$2" = -f && test "$3" = mmc-test && touch "$MMC_TEST_RUNTIME/gone" ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "container"), []byte(runtime), 0700); err != nil {
		t.Fatal(err)
	}
	s := appSession{}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	await := func(command, expected string) {
		t.Helper()
		for ctx.Err() == nil {
			if s.quitCommand(ctx, command) == expected {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("quit response did not reach %s", expected)
	}
	await("quit-prepare", "busy")
	await("quit-resume", "ready")
	await("quit-prepare", "busy")
	await("quit-stop", "ready")
}
