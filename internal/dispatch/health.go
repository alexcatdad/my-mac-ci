package dispatch

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alexcatdad/my-mac-ci/internal/isolation"
	"github.com/alexcatdad/my-mac-ci/internal/runnerimage"
	"github.com/alexcatdad/my-mac-ci/internal/scheduler"
)

// Health gives the supervising app a view of the current dispatch session.
type Health struct {
	controller         atomic.Pointer[Controller]
	quitMu             sync.Mutex
	draining, stopping bool
}
type sessionKey struct{}

func WithHealth(ctx context.Context, h *Health) context.Context {
	return context.WithValue(ctx, sessionKey{}, h)
}

func (h *Health) Controller() *Controller {
	if h == nil {
		return nil
	}
	return h.controller.Load()
}

type Snapshot struct {
	Labels    []LabelHealth   `json:"labels"`
	Isolation IsolationHealth `json:"isolation"`
}

type LabelHealth struct {
	Repo             string `json:"repo"`
	Label            string `json:"label"`
	Digest           string `json:"digest"`
	Starting         int    `json:"starting"`
	Running          int    `json:"running"`
	LastContact      int64  `json:"lastContact"`
	Stale            bool   `json:"stale"`
	ImageUnavailable bool   `json:"imageUnavailable"`
	StartFailed      bool   `json:"startFailed"`
	SessionConflict  bool   `json:"sessionConflict"`
	ListenerFailure  bool   `json:"listenerFailure"`
	VersionBehind    *bool  `json:"versionBehind,omitempty"`
}

type IsolationHealth struct {
	LastStatus isolation.Status        `json:"lastStatus,omitempty"`
	Status     isolation.Status        `json:"status"`
	CheckedAt  int64                   `json:"checkedAt"`
	Targets    []isolation.CheckResult `json:"targets"`
}

func (h *Health) Snapshot() *Snapshot {
	c := h.Controller()
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	s := &Snapshot{Labels: make([]LabelHealth, 0, len(c.labels)), Isolation: IsolationHealth{Status: c.status, Targets: []isolation.CheckResult{}}}
	if s.Isolation.Status == "" {
		s.Isolation.Status = isolation.Inconclusive
	}
	if c.report != nil {
		report, checked := c.report()
		s.Isolation.CheckedAt = checked
		s.Isolation.LastStatus = report.Status
		s.Isolation.Targets = append(s.Isolation.Targets, report.Checks...)
	}
	type counts struct{ starting, running int }
	active := make(map[scheduler.Key]counts)
	for _, r := range c.runners {
		n := active[r.key]
		if r.working {
			n.running++
		} else {
			n.starting++
		}
		active[r.key] = n
	}
	for key, l := range c.labels {
		_, digest, _ := strings.Cut(l.Image, "@")
		row := LabelHealth{Repo: key.Repo, Label: key.Label, Digest: digest, Starting: active[key].starting, Running: active[key].running,
			ImageUnavailable: l.imageUnavailable, StartFailed: l.startFailed, SessionConflict: l.conflict, ListenerFailure: l.listenerFailure, VersionBehind: l.versionBehind}
		since := l.lastContact
		if since.IsZero() {
			since = l.created
		} else {
			row.LastContact = since.Unix()
		}
		row.Stale = now.Sub(since) >= 5*time.Minute
		s.Labels = append(s.Labels, row)
	}
	slices.SortFunc(s.Labels, func(a, b LabelHealth) int { return cmp.Or(cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.Label, b.Label)) })
	return s
}

func (c *Controller) refreshVersions(ctx context.Context) {
	for ctx.Err() == nil {
		c.mu.Lock()
		refs := make([]string, 0, len(c.labels))
		for _, l := range c.labels {
			refs = append(refs, l.Image)
		}
		c.mu.Unlock()
		status := runnerimage.Check(ctx, refs)
		c.mu.Lock()
		for _, l := range c.labels {
			if v, ok := status[l.Image]; ok && v.Latest != "" && v.Version != "" {
				l.versionBehind = new(v.Outdated)
			}
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		case <-c.imagesChanged:
		}
	}
}
