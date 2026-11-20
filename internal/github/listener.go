package github

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

type Config struct {
	Repo           string // owner/repo, validated by the config loader
	Label          string
	MaxRunners     int // effective capacity for this label
	ClientID       string
	InstallationID int64
	PrivateKey     string
	Capacity       func() int
}

// Update is one poll, including idle heartbeats. A nil Assigned preserves the
// last known demand. Apply the whole batch before making any start decisions.
// GitHub may redeliver lifecycle events after a failed acknowledgement.
type Update struct {
	RemoveIdle func(context.Context, string) (bool, error)
	Assigned   *int
	Started    []Job
	Completed  []Job
	JIT        func(context.Context, string) (string, error)
}

// Job carries only the lifecycle fields needed by the scheduler and usage
// recorder. Workflow and Display must never be logged or written verbatim.
type Job struct {
	Runner, Workflow, Display, Event, Outcome string
	AssignedAt, FinishedAt                    time.Time
}

func jobEvent(name, result string, b scaleset.JobMessageBase) Job {
	ref, _, _ := strings.Cut(b.JobWorkflowRef, "@")
	workflow := ""
	if _, file, found := strings.Cut(ref, ".github/workflows/"); found && file != "" {
		workflow = ".github/workflows/" + file
	}
	outcome := "unknown"
	switch strings.ToLower(result) {
	case "succeeded", "success":
		outcome = "success"
	case "failed", "failure", "timed_out", "abandoned":
		outcome = "failure"
	case "canceled", "cancelled":
		outcome = "cancelled"
	}
	return Job{Runner: name, Workflow: workflow, Display: strings.TrimSpace(b.JobDisplayName), Event: b.EventName, Outcome: outcome,
		AssignedAt: b.RunnerAssignTime, FinishedAt: b.FinishTime}
}

var ErrSessionConflict = errors.New("scale set already has an active listener")
var errUnavailable = errors.New("scale set listener unavailable")

// Run maintains one label's listener until cancellation. Callbacks run serially.
// Errors exposed to the caller never include SDK request URLs or response bodies.
func Run(ctx context.Context, cfg Config, onUpdate func(Update), onError func(error)) error {
	if cfg.Label == "" || cfg.MaxRunners < 0 || cfg.MaxRunners > math.MaxInt32 || onUpdate == nil || onError == nil {
		return errors.New("invalid listener configuration")
	}
	info := scaleset.SystemInfo{System: "mmc", Subsystem: "listener"}
	client, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: "https://github.com/" + cfg.Repo,
		GitHubAppAuth: scaleset.GitHubAppAuth{
			ClientID: cfg.ClientID, InstallationID: cfg.InstallationID, PrivateKey: cfg.PrivateKey,
		},
		SystemInfo: info,
	})
	if err != nil {
		return errors.New("invalid GitHub App credentials or repository")
	}

	return listen(ctx, client, cfg, onUpdate, onError)
}

func listen(ctx context.Context, client *scaleset.Client, cfg Config, onUpdate func(Update), onError func(error)) error {
	delay := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := runSession(ctx, client, cfg, onUpdate)
		if time.Since(started) >= time.Minute {
			delay = time.Second
		}
		if ctx.Err() != nil {
			break
		}
		if errors.Is(err, ErrSessionConflict) {
			onError(ErrSessionConflict)
		} else {
			onError(errUnavailable)
		}
		wait := delay
		var limit *scaleset.RateLimitError
		if errors.As(err, &limit) {
			wait = max(wait, limit.RetryAfter)
			if limit.Kind == scaleset.RateLimitSecondary {
				wait = max(wait, time.Minute)
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		delay = min(2*delay, time.Minute)
	}
	return ctx.Err()
}

func runSession(ctx context.Context, client *scaleset.Client, cfg Config, onUpdate func(Update)) error {
	const groupID = 1 // default runner group used by repo-level scale sets
	set, err := client.GetRunnerScaleSet(ctx, groupID, cfg.Label)
	if err != nil {
		return err
	}
	if set == nil {
		set, err = client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name: cfg.Label, RunnerGroupID: groupID,
			Labels: []scaleset.Label{{Name: cfg.Label}}, RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		})
		if err != nil {
			return err
		}
	}
	info := client.SystemInfo()
	info.ScaleSetID = set.ID
	client.SetSystemInfo(info)
	session, err := client.MessageSessionClient(ctx, set.ID, "mmc-helper")
	if errors.Is(err, scaleset.ConflictError) {
		return ErrSessionConflict
	}
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = session.Close(closeCtx)
	}()

	jit := func(ctx context.Context, name string) (string, error) {
		// Checkout scopes Git credentials to the physical workspace path.
		result, err := client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{
			Name: name, WorkFolder: "/home/runner/_work",
		}, set.ID)
		if err != nil || result == nil || result.EncodedJITConfig == "" {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", errors.New("failed to generate JIT configuration")
		}
		return result.EncodedJITConfig, nil
	}
	return listenWithCapacity(ctx, session, set.ID, cfg.MaxRunners, jit, func(u Update) {
		u.RemoveIdle = func(ctx context.Context, name string) (bool, error) {
			return removeIdle(ctx, client, set.ID, name)
		}
		onUpdate(u)
	}, cfg.Capacity)
}

// Listen processes one open session. The caller owns session cleanup.
func Listen(ctx context.Context, session listener.Client, setID, maxRunners int, jit func(context.Context, string) (string, error), onUpdate func(Update)) error {
	return listenWithCapacity(ctx, session, setID, maxRunners, jit, onUpdate, nil)
}

func listenWithCapacity(ctx context.Context, session listener.Client, setID, maxRunners int, jit func(context.Context, string) (string, error), onUpdate func(Update), capacity func() int) error {
	if jit == nil || onUpdate == nil {
		return errors.New("invalid listener callbacks")
	}
	l, err := listener.New(session, listener.Config{ScaleSetID: setID, MaxRunners: maxRunners})
	if err != nil {
		return err
	}
	err = l.Run(ctx, &messages{session: session, jit: jit, capacity: capacity, onUpdate: func(u Update) {
		onUpdate(u)
		if capacity != nil {
			l.SetMaxRunners(capacity())
		}
	}})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var limit *scaleset.RateLimitError
	if errors.As(err, &limit) {
		return &scaleset.RateLimitError{Kind: limit.Kind, RetryAfter: limit.RetryAfter}
	}
	if err != nil {
		return errUnavailable
	}
	return nil
}

type messages struct {
	capacity func() int
	session  listener.Client
	jit      func(context.Context, string) (string, error)
	onUpdate func(Update)
}

func (m *messages) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	update := Update{JIT: m.jit}
	if msg != nil {
		var ids []int64
		for _, j := range msg.JobAvailableMessages {
			if j != nil {
				ids = append(ids, j.RunnerRequestID)
			}
		}
		if len(ids) > 0 && (m.capacity == nil || m.capacity() > 0) {
			if _, err := m.session.AcquireJobs(ctx, ids); err != nil {
				return err
			}
		}
		for _, j := range msg.JobStartedMessages {
			if j != nil && j.RunnerName != "" {
				update.Started = append(update.Started, jobEvent(j.RunnerName, "", j.JobMessageBase))
			}
		}
		for _, j := range msg.JobCompletedMessages {
			if j != nil && j.RunnerName != "" {
				update.Completed = append(update.Completed, jobEvent(j.RunnerName, j.Result, j.JobMessageBase))
			}
		}
		if msg.Statistics != nil {
			update.Assigned = &msg.Statistics.TotalAssignedJobs
		}
	}
	m.onUpdate(update)
	return nil
}
