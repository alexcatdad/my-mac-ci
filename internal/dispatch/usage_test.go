package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/alexcatdad/my-mac-ci/internal/github"
	"github.com/alexcatdad/my-mac-ci/internal/usage"
)

type finishMeasurementFunc func(string, usage.Job) error

func (f finishMeasurementFunc) Finish(dir string, job usage.Job) error { return f(dir, job) }
func (f finishMeasurementFunc) Discard()                               {}

func TestUsageTracksJobAcrossSessionAndLateCompletion(t *testing.T) {
	c := New(dispatchConfig(), nil)
	key := labelKey
	const name = "mmc-synthetic"
	r := &runner{key: key, launched: true, cancel: func() {}, job: allocation(key, dispatchConfig().Cap, "image@sha256:synthetic")}
	c.runners[name] = r
	started := time.Now().Add(-time.Minute)
	job := github.Job{Runner: name, Workflow: ".github/workflows/example.yml", Display: "check", Event: "push", AssignedAt: started}
	starts := 0
	finished, updated := make(chan usage.Job, 1), make(chan usage.Job, 1)
	proceed := make(chan struct{})
	var sampled context.Context
	c.measurements = &jobMeasurements{ctx: t.Context(), dir: t.TempDir(),
		start: func(ctx context.Context, _ string) measurement {
			starts++
			sampled = ctx
			return finishMeasurementFunc(func(_ string, saved usage.Job) error {
				<-proceed
				finished <- saved
				return nil
			})
		}, complete: func(_, _ string, job usage.Job) error {
			if len(finished) != 1 {
				t.Error("late completion ran before exit metrics were stored")
			}
			updated <- job
			return nil
		}}
	c.Update(key, github.Update{Started: []github.Job{job}})
	c.Update(key, github.Update{}) // Session loss is not job completion.
	c.Update(key, github.Update{Started: []github.Job{job}})
	if starts != 1 || sampled.Err() != nil {
		t.Fatal("reconnection changed sampling interval")
	}
	c.release(name)
	if sampled.Err() == nil {
		t.Fatal("sampler outlived VM exit")
	}
	end := started.Add(50 * time.Second)
	job.FinishedAt, job.Outcome = end, "cancelled"
	c.Update(key, github.Update{Completed: []github.Job{job}})
	close(proceed)
	c.closeMeasurements()
	before, after := <-finished, <-updated
	if before.Outcome != "unknown" || before.CPUs != r.job.CPUs || before.ImageDigest != "sha256:synthetic" ||
		before.Job != "check" || before.Event != "push" || before.StartedAt != started {
		t.Fatal("exit lost original job identity, timing or allocation")
	}
	if after.Outcome != "cancelled" || after.FinishedAt != end || after.StartedAt != started {
		t.Fatal("broker result did not supersede the local exit")
	}
}
