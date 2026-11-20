package usage

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestStatsContract(t *testing.T) {
	name := "mmc-fixture"
	got, ok := decode([]byte(`[{"id":"mmc-fixture","cpuUtilization":1.5,"memoryUsageBytes":2468}]`), name)
	if !ok || got != (sample{1.5, 2468}) {
		t.Fatal("incorrect stats units")
	}
	for _, bad := range []string{`[]`, `[{"id":"foreign","cpuUtilization":1,"memoryUsageBytes":1}]`, `[{"id":"mmc-fixture","cpuUsageUsec":3,"memoryLimitBytes":7}]`, `[{"id":"mmc-fixture","cpuUtilization":null,"memoryUsageBytes":9}]`, `[{"id":"mmc-fixture","cpuUtilization":-1,"memoryUsageBytes":9}]`} {
		if _, ok := decode([]byte(bad), name); ok {
			t.Fatal("accepted incomplete or unrelated sample")
		}
	}
}

func TestSampleCoverageStopsAtCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		r := start(t.Context(), "mmc-fixture", func(ctx context.Context, _ string) (sample, bool) {
			calls++
			select {
			case <-ctx.Done():
				return sample{}, false
			case <-time.After(2 * time.Second):
			}
			return sample{float64(calls), uint64(calls)}, calls != 3
		})
		time.Sleep(18 * time.Second)
		r.Discard()
		time.Sleep(time.Minute)
		got := r.record
		if calls != 4 || got.Samples != 3 || got.Missed != 1 || got.CoveredMillis != 9000 || got.PeakCores != 4 || got.AverageCores != 7.0/3 || got.PeakMemoryBytes != 4 {
			t.Fatalf("unexpected aggregate %+v", got)
		}
	})
}

func TestStoreRedeliveryRecoveryAndPrivacy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	now := time.Now().UTC()
	j := Job{Repo: "synthetic/repo", Label: "synthetic-label", Workflow: "synthetic-workflow", Job: "synthetic-job", CPUs: 2, MemoryBytes: 4096, ImageDigest: "registry.invalid/hidden@sha256:" + strings.Repeat("a", 64)}
	const runner = "mmc-synthetic"
	j.Event = "synthetic-private-event"
	first := Record{StartedAt: now, FinishedAt: now.Add(2 * time.Second), SampledFrom: now, SampledThrough: now.Add(2 * time.Second), CoveredMillis: 2000, Samples: 1, AverageCores: 1, PeakCores: 1, PeakMemoryBytes: 400}
	if err := Complete(dir, runner, j); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("completion created idle store")
	}
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			if err := update(dir, runner, j, &first); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	second := first
	second.SampledFrom = now.Add(time.Minute)
	second.SampledThrough = second.SampledFrom.Add(2 * time.Second)
	second.FinishedAt = second.SampledThrough
	second.AverageCores, second.PeakCores, second.PeakMemoryBytes = 2, 2, 700
	if err := update(dir, runner, j, &second); err != nil {
		t.Fatal(err)
	}
	j.Outcome = "success"
	j.Event = "schedule"
	j.FinishedAt = second.FinishedAt
	if err := Complete(dir, runner, j); err != nil {
		t.Fatal(err)
	}
	j.Outcome = "unknown"
	j.Event = ""
	if err := update(dir, runner, j, &first); err != nil {
		t.Fatal(err)
	}
	out, err := Read(dir)
	if err != nil || len(out) != 1 {
		t.Fatal("cannot read one runner summary", err)
	}
	r := out[0]
	if r.Samples != 2 || r.CoveredMillis != 4000 || r.AverageCores != 1.5 || r.PeakMemoryBytes != 700 || r.PeakCores != 2 || r.Outcome != "success" || r.Event != "schedule" || r.Group != WorkloadGroup(j.Repo, j.Label, j.Workflow, j.Job) || !r.WorkloadKnown {
		t.Fatal("lost or duplicated observations")
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatal("unexpected usage files")
	}
	path := filepath.Join(dir, files[0].Name())
	data, err := os.ReadFile(path)
	if err != nil || bytes.Contains(data, []byte("synthetic")) || bytes.Contains(data, []byte("registry.invalid")) {
		t.Fatal("raw identity or registry escaped")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("unsafe summary permissions")
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("accepted unsafe directory")
	}
}
