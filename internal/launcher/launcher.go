// Package launcher runs one job VM and confirms its removal before releasing
// the caller's resource reservation.
package launcher

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/alexcatdad/my-mac-ci/internal/config"
	"github.com/alexcatdad/my-mac-ci/internal/scheduler"
)

const (
	ownershipLabel = "com.alexcatdad.mmc.runner"
	repoLabel      = "com.alexcatdad.mmc.repo"
	runnerLabel    = "com.alexcatdad.mmc.label"
	digestLabel    = "com.alexcatdad.mmc.image-digest"
	callTimeout    = 10 * time.Second
	startupTimeout = time.Minute
	// Attached run covers GitHub's 5-day self-hosted job limit and its
	// 24-hour queue window. Short startup and management calls are independent.
	maxLifetime = 6 * 24 * time.Hour
)

var (
	runnerName  = regexp.MustCompile(`^mmc-[a-zA-Z0-9][a-zA-Z0-9_.-]{0,58}$`)
	imageDigest = regexp.MustCompile(`^[a-zA-Z0-9][^@[:space:]]*@sha256:[a-f0-9]{64}$`)
	errRun      = errors.New("runner container failed")
)

// Start returns an immediate error only if it could not launch the CLI. Once
// launched, the channel emits exactly one result after the VM is confirmed
// absent. Cancel ctx to stop this runner. The caller supplies a unique name.
func Start(ctx context.Context, name string, key scheduler.Key, image string, resources config.Resources, jit string, authorize func(func() error) error) (<-chan error, error) {
	if key.Repo == "" || key.Label == "" || !runnerName.MatchString(name) || !imageDigest.MatchString(image) || resources.CPUs < 1 ||
		authorize == nil || resources.Memory < 1 || jit == "" ||
		len(jit) >= 1<<20 || strings.ContainsAny(jit, "\r\n") {
		return nil, errors.New("invalid runner launch parameters")
	}
	if _, err := management(ctx, "image", "inspect", image); err != nil {
		return nil, errors.New("runner image unavailable locally")
	}
	nonce := rand.Text()
	_, digest, _ := strings.Cut(image, "@")
	runCtx, cancel := context.WithTimeout(ctx, maxLifetime)
	cmd := exec.CommandContext(runCtx, "container", "run", "--rm", "-i", "--name", name,
		"--label", ownershipLabel+"="+nonce, "--label", repoLabel+"="+key.Repo, "--label", runnerLabel+"="+key.Label, "--label", digestLabel+"="+digest,
		"--cpus", strconv.Itoa(resources.CPUs),
		"--memory", strconv.FormatInt(resources.Memory, 10)+"b", "--network", "mmc",
		"--dns", "1.1.1.1", "--kernel-arg", "ipv6.disable=1", "--tmpfs", "/run/mmc",
		"--entrypoint", "/usr/local/bin/mmc-runner", image)
	// The CLI copies stdin to the guest. Guest output is discarded.
	cmd.Stdin = strings.NewReader(jit + "\n")
	cmd.WaitDelay = time.Second
	if err := authorize(cmd.Start); err != nil {
		cancel()
		if errors.Is(err, context.Canceled) {
			return nil, context.Canceled
		}
		return nil, errors.New("could not launch container runtime")
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	done := make(chan error, 1)
	go func() {
		defer close(done)
		defer cancel()
		timer := time.NewTimer(startupTimeout)
		defer timer.Stop()
		var result error
		select {
		case result = <-wait:
		case <-timer.C:
			// A queued run with a wedged CLI must not hold a slot for the
			// entire job lifetime before diagnosing a startup failure.
			state, err := inspect(runCtx, name)
			if err != nil || state.owner != nonce || state.status != "running" {
				cancel()
			}
			result = <-wait
		}
		if result != nil || runCtx.Err() != nil {
			result = errRun
		}
		waitForRemoval(context.WithoutCancel(runCtx), runCtx, name, nonce, result != nil)
		if runCtx.Err() != nil {
			result = errRun
		}
		done <- result
	}()
	return done, nil
}

type state struct {
	present bool
	owner   string
	status  string
}

type snapshot struct {
	ID            string `json:"id"`
	Configuration struct {
		ID        string            `json:"id"`
		Labels    map[string]string `json:"labels"`
		Resources struct {
			CPUs   int   `json:"cpus"`
			Memory int64 `json:"memoryInBytes"`
		} `json:"resources"`
	} `json:"configuration"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
}

func list(ctx context.Context) ([]snapshot, error) {
	b, err := management(ctx, "list", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	var rows []snapshot
	if json.Unmarshal(b, &rows) != nil || rows == nil {
		return nil, errors.New("invalid container inventory")
	}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.ID == "" || row.ID != row.Configuration.ID || row.Status.State == "" || row.Configuration.Labels == nil || seen[row.ID] {
			return nil, errors.New("inconsistent container inventory")
		}
		seen[row.ID] = true
	}
	return rows, nil
}

func inspect(ctx context.Context, name string) (state, error) {
	rows, err := list(ctx)
	if err != nil {
		return state{}, err
	}
	for _, row := range rows {
		if row.ID == name {
			return state{true, row.Configuration.Labels[ownershipLabel], row.Status.State}, nil
		}
	}
	return state{}, nil
}

// Survivor identifies a previously launched VM and the actual resources it
// still holds. Current config may have different limits or no such label.
type Survivor struct {
	Name        string
	ImageDigest string
	Key         scheduler.Key
	Resources   config.Resources
	owner       string
}

// Inventory returns all MMC-owned containers, including those awaiting
// removal. An owner marker without complete provenance blocks recovery until
// that VM exits. Unmarked containers are never adopted.
func Inventory(ctx context.Context) ([]Survivor, error) {
	rows, err := list(ctx)
	if err != nil {
		return nil, err
	}
	var survivors []Survivor
	for _, row := range rows {
		labels := row.Configuration.Labels
		nonce, marked := labels[ownershipLabel]
		if !marked {
			continue
		}
		key := scheduler.Key{Repo: labels[repoLabel], Label: labels[runnerLabel]}
		r := row.Configuration.Resources
		if nonce == "" || !runnerName.MatchString(row.ID) || key.Repo == "" || key.Label == "" || r.CPUs < 1 || r.Memory < 1 {
			return nil, errors.New("owned runner has incomplete recovery metadata")
		}
		digest := labels[digestLabel]
		if !imageDigest.MatchString("image@" + digest) {
			digest = ""
		}
		survivors = append(survivors, Survivor{Name: row.ID, Key: key, Resources: config.Resources{CPUs: r.CPUs, Memory: r.Memory}, owner: nonce, ImageDigest: digest})
	}
	return survivors, nil
}

// Wait returns true when the VM is absent. Cancel stop to request removal of
// this instance, or cancel ctx to detach the monitor without stopping a job.
func (s Survivor) Wait(ctx, stop context.Context) bool {
	return waitForRemoval(ctx, stop, s.Name, s.owner, false)
}

func waitForRemoval(ctx, stop context.Context, name, nonce string, failed bool) bool {
	stopSignal := stop.Done()
	for delay := 250 * time.Millisecond; ctx.Err() == nil; delay = min(2*delay, 10*time.Second) {
		s, err := inspect(ctx, name)
		if err == nil && ctx.Err() == nil {
			if !s.present {
				return true
			}
			if nonce != "" && s.owner == nonce && (failed || stop.Err() != nil || s.status == "stopped") {
				_, _ = management(ctx, "rm", "-f", name)
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-stopSignal:
			stopSignal = nil
		case <-time.After(delay):
		}
	}
	return false
}

func management(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "container", args...)
	cmd.WaitDelay = time.Second
	b, err := cmd.Output()
	if err != nil {
		return nil, errors.New("container runtime unavailable")
	}
	return b, nil
}

// EnsureImage uses an existing digest pin offline or pulls that exact digest.
func EnsureImage(ctx context.Context, ref string) error {
	if !imageDigest.MatchString(ref) {
		return errors.New("invalid runner image digest")
	}
	if _, err := management(ctx, "image", "inspect", ref); err == nil {
		return nil
	}
	pull, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(pull, "container", "image", "pull", ref)
	cmd.WaitDelay = time.Second
	if cmd.Run() != nil {
		return errors.New("runner image pull failed")
	}
	_, err := management(ctx, "image", "inspect", ref)
	return err
}
