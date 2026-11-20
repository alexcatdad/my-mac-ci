// Package usage samples only active runners and retains aggregate measurements.
package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const interval = 5 * time.Second

var runnerID = regexp.MustCompile(`^mmc-[a-zA-Z0-9][a-zA-Z0-9_.-]{0,58}$`)

type Job struct {
	Repo, Label, Workflow, Job, ImageDigest, Outcome, Event string
	CPUs                                                    int
	MemoryBytes                                             int64
	StartedAt, FinishedAt                                   time.Time
}

type Record struct {
	Group           string    `json:"group"`
	LabelGroup      string    `json:"labelGroup"`
	WorkloadKnown   bool      `json:"workloadKnown"`
	ImageDigest     string    `json:"imageDigest,omitempty"`
	CPUs            int       `json:"cpus"`
	MemoryBytes     int64     `json:"memoryBytes"`
	Outcome         string    `json:"outcome"`
	Event           string    `json:"event"`
	StartedAt       time.Time `json:"startedAt"`
	FinishedAt      time.Time `json:"finishedAt"`
	Samples         int       `json:"samples"`
	Missed          int       `json:"missed"`
	SampledFrom     time.Time `json:"sampledFrom"`
	SampledThrough  time.Time `json:"sampledThrough"`
	CoveredMillis   int64     `json:"coveredMillis"`
	PeakCores       float64   `json:"peakCores"`
	AverageCores    float64   `json:"averageCores"`
	PeakMemoryBytes uint64    `json:"peakMemoryBytes"`
}

func hash(parts ...string) string {
	// JSON strings are length-unambiguous even when job names contain separators.
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func LabelGroup(repo, label string) string {
	return hash(strings.ToLower(repo), strings.ToLower(label))
}
func Workload(workflow, job string) string { return hash(workflow, job) }
func WorkloadGroup(repo, label, workflow, job string) string {
	return hash(LabelGroup(repo, label), Workload(workflow, job))
}

type sample struct {
	cpu    float64
	memory uint64
}
type Run struct {
	name    string
	started time.Time
	stopped time.Time
	cancel  context.CancelFunc
	done    chan struct{}
	record  Record // frozen after done closes
}

// Start uses Apple container's two-second, single-report sampling mode.
// No process or ticker exists outside the lifetime of this runner.
func Start(ctx context.Context, runner string) *Run {
	return start(ctx, runner, stats)
}

func start(ctx context.Context, name string, read func(context.Context, string) (sample, bool)) *Run {
	ctx, cancel := context.WithCancel(ctx)
	r := &Run{name: name, started: time.Now().UTC(), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer func() { r.stopped = time.Now().UTC(); close(r.done) }()
		if !runnerID.MatchString(name) {
			return
		}
		tick := time.NewTicker(interval)
		defer tick.Stop()
		var last time.Time
		for ctx.Err() == nil {
			begin := time.Now().UTC()
			bounded, stop := context.WithTimeout(ctx, 4*time.Second)
			s, ok := read(bounded, name)
			stop()
			if ok {
				rec := &r.record
				if rec.Samples == 0 {
					rec.SampledFrom = begin
				}
				rec.SampledThrough = time.Now().UTC()
				window := min(2*time.Second, rec.SampledThrough.Sub(begin))
				if !last.IsZero() && rec.SampledThrough.Sub(last) <= interval+2*time.Second {
					window = min(interval, rec.SampledThrough.Sub(last))
				}
				rec.CoveredMillis += window.Milliseconds()
				last = rec.SampledThrough
				rec.Samples++
				rec.PeakCores = max(rec.PeakCores, s.cpu)
				rec.AverageCores += (s.cpu - rec.AverageCores) / float64(rec.Samples)
				rec.PeakMemoryBytes = max(rec.PeakMemoryBytes, s.memory)
			} else if ctx.Err() == nil {
				r.record.Missed++
				last = time.Time{}
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return r
}

type limitedOutput struct{ buf bytes.Buffer }

func (o *limitedOutput) Write(p []byte) (int, error) {
	if o.buf.Len()+len(p) > 16384 {
		return 0, errors.New("stats output exceeded limit")
	}
	return o.buf.Write(p)
}

func stats(ctx context.Context, name string) (sample, bool) {
	cmd := exec.CommandContext(ctx, "container", "stats", "--no-stream", "--format", "json", name)
	cmd.WaitDelay = 500 * time.Millisecond
	var out limitedOutput
	cmd.Stdout = &out
	if cmd.Run() != nil {
		return sample{}, false
	}
	return decode(out.buf.Bytes(), name)
}

func decode(b []byte, name string) (sample, bool) {
	// apple/container 1.7.1 ContainerStats.StatsReport uses fractional cores
	// (1.0 is one busy core) and physical bytes. Both measurements are optional.
	var rows []struct {
		ID               string
		CPUUtilization   *float64
		MemoryUsageBytes *uint64
	}
	if json.Unmarshal(b, &rows) != nil || len(rows) != 1 || rows[0].ID != name {
		return sample{}, false
	}
	row := rows[0]
	if row.CPUUtilization == nil || *row.CPUUtilization < 0 || row.MemoryUsageBytes == nil {
		return sample{}, false
	}
	return sample{*row.CPUUtilization, *row.MemoryUsageBytes}, true
}

func (r *Run) Discard() { r.cancel(); <-r.done }

func (r *Run) Finish(dir string, j Job) error {
	r.Discard()
	rec := r.record
	rec.StartedAt = r.started
	rec.FinishedAt = r.stopped
	return update(dir, r.name, j, &rec)
}

// Complete fills in broker metadata for an already recorded runner.
func Complete(dir, runner string, j Job) error { return update(dir, runner, j, nil) }
