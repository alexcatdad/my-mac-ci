package dispatch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alexcatdad/my-mac-ci/internal/github"
	"github.com/alexcatdad/my-mac-ci/internal/isolation"
)

func TestHealthReportsJobContactAndCountsWithoutImageReference(t *testing.T) {
	cfg := dispatchConfig()
	label := cfg.Repos[labelKey.Repo][labelKey.Label]
	digest := "sha256:" + strings.Repeat("1", 64)
	label.Image = "runner@" + digest
	cfg.Repos[labelKey.Repo][labelKey.Label] = label
	c := New(cfg, nil)
	c.labels[labelKey].created = time.Now().Add(-5 * time.Minute)
	c.runners["runner"] = &runner{key: labelKey}
	h := new(Health)
	h.controller.Store(c)
	before := h.Snapshot().Labels[0]
	if !before.Stale || before.LastContact != 0 || before.Starting != 1 || before.Running != 0 {
		t.Fatal(before)
	}
	c.Update(labelKey, github.Update{JIT: func(context.Context, string) (string, error) { return "", nil }, Started: []github.Job{{Runner: "runner"}}})
	c.report = func() (isolation.Report, int64) {
		return isolation.Report{Status: isolation.Pass, Checks: []isolation.CheckResult{{Name: "host LAN", Status: isolation.Pass}}}, 42
	}
	s := h.Snapshot()
	row := s.Labels[0]
	if row.Stale || row.LastContact == 0 || row.Starting != 0 || row.Running != 1 || row.Digest != digest || s.Isolation.CheckedAt != 42 || s.Isolation.LastStatus != isolation.Pass {
		t.Fatal("incorrect health summary")
	}
	b, err := json.Marshal(s)
	if err != nil || strings.Contains(string(b), cfg.Registry) || strings.Contains(string(b), "runner@") {
		t.Fatal("private image reference escaped")
	}
}
