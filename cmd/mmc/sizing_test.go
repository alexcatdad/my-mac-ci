package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/alexcatdad/my-mac-ci/internal/usage"
)

func TestSizingReviewRequiresComparableMeasuredSuccesses(t *testing.T) {
	start := time.Unix(1000, 0)
	base := usage.Record{LabelGroup: "label", Group: "job", WorkloadKnown: true, ImageDigest: "digest-a", CPUs: 2, MemoryBytes: 1 << 30, Samples: 4, CoveredMillis: 15000, StartedAt: start, FinishedAt: start.Add(20 * time.Second), Outcome: "success", AverageCores: 0.5, PeakCores: 0.8, PeakMemoryBytes: 256 << 20}
	base.SampledFrom, base.SampledThrough = base.StartedAt, base.FinishedAt
	base.Event = "pull_request"
	var records []usage.Record
	for range 34 {
		records = append(records, base)
	}
	for _, change := range []func(*usage.Record){
		func(r *usage.Record) { r.ImageDigest = "digest-b" }, func(r *usage.Record) { r.CPUs++ }, func(r *usage.Record) { r.MemoryBytes++ },
		func(r *usage.Record) { r.Group = "another job" }, func(r *usage.Record) { r.LabelGroup = "another label" },
		func(r *usage.Record) { r.Event = "push" }, func(r *usage.Record) { r.Event = "unknown" },
		func(r *usage.Record) { r.Samples = 2 }, func(r *usage.Record) { r.CoveredMillis = 13999 },
		func(r *usage.Record) { r.Outcome = "failure"; r.PeakMemoryBytes = 1 << 30 }, func(r *usage.Record) { r.WorkloadKnown = false },
	} {
		r := base
		change(&r)
		records = append(records, r)
	}
	report := func() string {
		t.Helper()
		var out bytes.Buffer
		if err := writeSizing(&out, records, map[string]string{"label": "owner/example / ci"}); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if s := report(); !strings.Contains(s, "34 eligible successes; 1 failed, 0 other outcomes, 2 insufficiently sampled") || strings.Contains(s, "CPU mean p95") {
		t.Fatal(s)
	}
	records = append(records, base)
	s := report()
	for _, want := range []string{"35 eligible successes; 1 failed", "event pull_request", "CPU mean p95 25.0%, CPU peak p95 40.0%", "max 256.0 MiB (25.0%)", "median 20.0s", "Inspect unsuccessful", "2 excluded"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in %s", want, s)
		}
	}
	// A much older busy run cannot skew the most recent 35 eligible completions.
	old := base
	old.StartedAt = old.StartedAt.Add(-time.Hour)
	old.FinishedAt = old.FinishedAt.Add(-time.Hour)
	old.SampledFrom, old.SampledThrough = old.StartedAt, old.FinishedAt
	old.PeakCores = 2
	old.PeakMemoryBytes = 1 << 30
	records = append(records, old)
	if s := report(); strings.Contains(s, "max 100.0%") || strings.Contains(s, "max 1024.0 MiB") {
		t.Fatal(s)
	}
	records = make([]usage.Record, reviewJobs)
	for i := range records {
		records[i] = base
	}
	if s := report(); !strings.Contains(s, "smaller trial allocation") {
		t.Fatal(s)
	}
	for i := range records {
		records[i].PeakCores = 1.9
	}
	if s := report(); !strings.Contains(s, "larger allocation") {
		t.Fatal(s)
	}
}
