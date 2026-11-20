// mmc-trial: actions/scaleset examples/dockerscaleset, adapted to Apple `container`.
// One scale set, one container per job, JIT config over stdin. Trial only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
)

var log = slog.New(slog.NewTextHandler(os.Stdout, nil))

func main() {
	url := flag.String("url", "", "repo URL, e.g. https://github.com/owner/repo")
	name := flag.String("name", "", "scale set name (= runs-on label)")
	image := flag.String("image", "", "runner image")
	cpus := flag.String("cpus", "2", "CPUs per container")
	memory := flag.String("memory", "2g", "memory per container")
	maxRunners := flag.Int("max", 1, "max runners")
	flag.Parse()
	if *url == "" || *name == "" || *image == "" {
		flag.Usage()
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, *url, *name, &Scaler{image: *image, cpus: *cpus, memory: *memory, max: *maxRunners, runners: map[string]*runner{}}); err != nil {
		log.Error("exit", "error", err)
		os.Exit(1)
	}
}

func env(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Error("missing env", "name", k)
		os.Exit(2)
	}
	return v
}

func run(ctx context.Context, url, name string, s *Scaler) error {
	key, err := os.ReadFile(env("MMC_GITHUB_APP_KEY_FILE"))
	if err != nil {
		return err
	}
	inst, err := strconv.ParseInt(env("MMC_GITHUB_APP_INSTALLATION_ID"), 10, 64)
	if err != nil {
		return err
	}
	info := scaleset.SystemInfo{System: "mmc-trial", Subsystem: "mmc-trial", Version: "0.0.1", CommitSHA: "NA"}
	client, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: url,
		GitHubAppAuth:   scaleset.GitHubAppAuth{ClientID: env("MMC_GITHUB_APP_CLIENT_ID"), InstallationID: inst, PrivateKey: string(key)},
		SystemInfo:      info,
	})
	if err != nil {
		return fmt.Errorf("client: %w", err)
	}
	// Reuse a scale set left by an unclean exit: creating one with the same name fails
	// (RunnerScaleSetExistsException).
	set, err := client.GetRunnerScaleSet(ctx, 1, name)
	if err != nil {
		return fmt.Errorf("get scale set: %w", err)
	}
	if set == nil {
		set, err = client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name:          name,
			RunnerGroupID: 1,
			Labels:        []scaleset.Label{{Name: name}},
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		})
		if err != nil {
			return fmt.Errorf("create scale set: %w", err)
		}
		log.Info("scale set created", "id", set.ID, "name", name)
	} else {
		log.Info("scale set reused", "id", set.ID, "name", name)
	}
	defer func() {
		if err := client.DeleteRunnerScaleSet(context.WithoutCancel(ctx), set.ID); err != nil {
			log.Error("delete scale set", "error", err)
		}
	}()
	info.ScaleSetID = set.ID
	client.SetSystemInfo(info)

	host, _ := os.Hostname()
	// After an unclean exit GitHub keeps the old session (409 on a new one). Deleting the
	// scale set and creating it again clears that at once; queued jobs move to the new set.
	session, err := client.MessageSessionClient(ctx, set.ID, host)
	if errors.Is(err, scaleset.ConflictError) {
		log.Info("session conflict: recreating scale set", "id", set.ID)
		if err := client.DeleteRunnerScaleSet(ctx, set.ID); err != nil {
			return fmt.Errorf("delete stale scale set: %w", err)
		}
		set, err = client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name:          name,
			RunnerGroupID: 1,
			Labels:        []scaleset.Label{{Name: name}},
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		})
		if err != nil {
			return fmt.Errorf("recreate scale set: %w", err)
		}
		info.ScaleSetID = set.ID
		client.SetSystemInfo(info)
		session, err = client.MessageSessionClient(ctx, set.ID, host)
	}
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	defer session.Close(context.WithoutCancel(ctx))

	l, err := listener.New(session, listener.Config{ScaleSetID: set.ID, MaxRunners: s.max, Logger: log.WithGroup("listener")})
	if err != nil {
		return err
	}
	s.client, s.session, s.setID = client, session, set.ID
	go s.sampleMemory(ctx)
	defer s.shutdown()
	if err := l.Run(ctx, s); !errors.Is(err, context.Canceled) {
		return fmt.Errorf("listener: %w", err)
	}
	return nil
}

type runner struct {
	started, jobStarted time.Time
	peakMem             int64
	done                bool
}

type Scaler struct {
	image, cpus, memory string
	max, setID          int
	client              *scaleset.Client
	session             listener.Client
	mu                  sync.Mutex
	runners             map[string]*runner
}

// count excludes runners whose job completed: GitHub sends the new job count in the same
// message as JobCompleted and sends nothing when the container exits afterwards.
func (s *Scaler) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.runners {
		if !r.done {
			n++
		}
	}
	return n
}

func (s *Scaler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if msg == nil {
		return nil
	}
	if len(msg.JobAvailableMessages) > 0 {
		ids := make([]int64, 0, len(msg.JobAvailableMessages))
		for _, j := range msg.JobAvailableMessages {
			ids = append(ids, j.RunnerRequestID)
		}
		if _, err := s.session.AcquireJobs(ctx, ids); err != nil {
			return fmt.Errorf("acquire jobs: %w", err)
		}
	}
	for _, j := range msg.JobStartedMessages {
		s.mu.Lock()
		if r := s.runners[j.RunnerName]; r != nil {
			r.jobStarted = time.Now()
			log.Info("job started", "runner", j.RunnerName, "job", j.JobID, "runner_to_job", r.jobStarted.Sub(r.started).Round(time.Millisecond))
		}
		s.mu.Unlock()
	}
	for _, j := range msg.JobCompletedMessages {
		log.Info("job completed", "runner", j.RunnerName, "result", j.Result)
		if j.RunnerName != "" {
			s.mu.Lock()
			if r := s.runners[j.RunnerName]; r != nil {
				r.done = true
			}
			s.mu.Unlock()
			go s.removeAfter(j.RunnerName, 30*time.Second)
		}
	}
	if msg.Statistics == nil {
		return nil
	}
	want := min(s.max, msg.Statistics.TotalAssignedJobs)
	for have := s.count(); have < want; have++ {
		if err := s.start(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scaler) start(ctx context.Context) error {
	name := "mmc-" + uuid.NewString()[:8]
	jit, err := s.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, s.setID)
	if err != nil {
		return fmt.Errorf("jit config: %w", err)
	}
	cmd := exec.Command("container", "run", "--rm", "-i", "--name", name, "--cpus", s.cpus, "--memory", s.memory,
		"--entrypoint", "/usr/local/bin/mmc-runner", "--tmpfs", "/run/mmc", "--network", "mmc", "--dns", "1.1.1.1", "--kernel-arg", "ipv6.disable=1", s.image)
	cmd.Stdin = strings.NewReader(jit.EncodedJITConfig + "\n")
	out, err := os.Create(name + ".log")
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = out, out
	r := &runner{started: time.Now()}
	if err := cmd.Start(); err != nil {
		out.Close()
		return fmt.Errorf("container run: %w", err)
	}
	s.mu.Lock()
	s.runners[name] = r
	s.mu.Unlock()
	log.Info("runner starting", "runner", name)
	go func() {
		err := cmd.Wait()
		out.Close()
		s.mu.Lock()
		log.Info("container exited", "runner", name, "error", err, "lifetime", time.Since(r.started).Round(time.Millisecond), "peak_guest_mem_mb", r.peakMem>>20)
		delete(s.runners, name)
		s.mu.Unlock()
	}()
	return nil
}

// removeAfter lets the runner exit by itself after its job; force-removes it only if it is still there.
func (s *Scaler) removeAfter(name string, grace time.Duration) {
	time.Sleep(grace)
	s.mu.Lock()
	_, alive := s.runners[name]
	s.mu.Unlock()
	if alive {
		s.remove(name, "still running after job")
	}
}

func (s *Scaler) remove(name, why string) {
	log.Info("removing runner", "runner", name, "why", why)
	_ = exec.Command("container", "rm", "-f", name).Run()
}

func (s *Scaler) shutdown() {
	s.mu.Lock()
	names := make([]string, 0, len(s.runners))
	for n := range s.runners {
		names = append(names, n)
	}
	s.mu.Unlock()
	for _, n := range names {
		s.remove(n, "shutdown")
	}
}

// sampleMemory records each runner's peak guest memory every 2 s.
func (s *Scaler) sampleMemory(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if s.count() == 0 {
			continue
		}
		b, err := exec.Command("container", "stats", "--no-stream", "--format", "json").Output()
		if err != nil {
			continue
		}
		var stats []struct {
			ID  string `json:"id"`
			Mem int64  `json:"memoryUsageBytes"`
		}
		if json.Unmarshal(b, &stats) != nil {
			continue
		}
		s.mu.Lock()
		for _, st := range stats {
			if r := s.runners[st.ID]; r != nil && st.Mem > r.peakMem {
				r.peakMem = st.Mem
			}
		}
		s.mu.Unlock()
	}
}
