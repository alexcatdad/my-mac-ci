package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexcatdad/my-mac-ci/internal/config"
	"github.com/alexcatdad/my-mac-ci/internal/github"
	"github.com/alexcatdad/my-mac-ci/internal/isolation"
	"github.com/alexcatdad/my-mac-ci/internal/scheduler"
)

func TestReloadDrainsRemovedListenerAndPausesMissingDigest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	t.Setenv("MMC_TEST_CACHE", dir)
	if err := os.WriteFile(filepath.Join(dir, "container"), []byte(`#!/bin/sh
case "$1 $2" in
"image inspect") test -f "$MMC_TEST_CACHE/cached" ;;
"image pull") test -f "$MMC_TEST_CACHE/available" && touch "$MMC_TEST_CACHE/cached" ;;
*) exit 1 ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cfg := dispatchConfig()
	c := New(cfg, nil)
	h := new(Health)
	h.controller.Store(c)
	c.status = isolation.Pass
	monitor := &testMonitor{}
	monitor.fresh.Store(true)
	c.runners["existing"] = &runner{key: labelKey, launched: true, working: true, cancel: func() {}}
	if err := c.scheduler.Adopt("existing", labelKey, scheduler.Resources(cfg.Cap)); err != nil {
		t.Fatal(err)
	}
	started := make(chan github.Config, 4)
	exited := make(chan string, 4)
	listen := func(ctx context.Context, g github.Config, _ func(github.Update), _ func(error)) error {
		started <- g
		<-ctx.Done()
		exited <- g.Label
		return ctx.Err()
	}
	var workers sync.WaitGroup
	done := make(chan error, 1)
	go func() { done <- c.serveListeners(ctx, cfg, github.Config{}, monitor, listen, &workers, nil) }()
	defer func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Error("reload service did not stop cleanly")
		}
		workers.Wait()
	}()
	original := receive(t, started)
	replacement := dispatchConfig()
	next := replacement.Repos[labelKey.Repo][labelKey.Label]
	next.Image = "runner@sha256:" + strings.Repeat("b", 64)
	replacement.Repos[labelKey.Repo] = map[string]config.Label{"next": next}
	if err := h.Reload(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if original.Capacity() != 0 {
		t.Fatal("retired listener still acquiring jobs")
	}
	select {
	case <-exited:
		t.Fatal("retired listener exited before its job")
	default:
	}
	added := receive(t, started)
	wait := func(check func() bool) {
		t.Helper()
		for !check() {
			select {
			case <-ctx.Done():
				t.Fatal("timed out waiting for reload state")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	wait(func() bool {
		for _, r := range h.Snapshot().Labels {
			if r.Label == "next" {
				return r.ImageUnavailable
			}
		}
		return false
	})
	if added.Capacity() != 0 {
		t.Fatal("unavailable replacement digest accepted demand")
	}
	c.mu.Lock()
	c.release("existing")
	c.mu.Unlock()
	if label := receive(t, exited); label != labelKey.Label {
		t.Fatal("incorrect listener retired")
	}
	if err := os.WriteFile(filepath.Join(dir, "available"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { return added.Capacity() == 1 })
	row := h.Snapshot().Labels
	if len(row) != 1 || row[0].Digest != "sha256:"+strings.Repeat("b", 64) || row[0].ImageUnavailable {
		t.Fatal("unexpected replacement snapshot")
	}
}
