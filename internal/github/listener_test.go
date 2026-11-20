package github

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

type acquisition struct {
	listener.Client
	ids []int64
	err error
}

func (a *acquisition) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	a.ids = ids
	return nil, a.err
}

func TestScalePublishesBatchAfterAcquisition(t *testing.T) {
	var updates []Update
	jobs := &acquisition{err: errors.New("acquisition failed")}
	m := messages{
		session:  jobs,
		jit:      func(context.Context, string) (string, error) { return "synthetic", nil },
		onUpdate: func(u Update) { updates = append(updates, u) },
	}
	msg := &scaleset.RunnerScaleSetMessage{
		JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 7}}},
		JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "previous"}},
		JobStartedMessages: []*scaleset.JobStarted{{RunnerName: "current", JobMessageBase: scaleset.JobMessageBase{
			JobWorkflowRef: "example/test/.github/workflows/ci.yml@refs/heads/main", JobDisplayName: "test"}}},
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2},
	}
	if err := m.Scale(t.Context(), msg); err == nil || len(updates) != 0 {
		t.Fatal("failed acquisition must defer publication until redelivery")
	}
	jobs.err = nil
	if err := m.Scale(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(jobs.ids, []int64{7}) || len(updates) != 1 {
		t.Fatal("expected one acquisition and one update")
	}
	u := updates[0]
	if u.Assigned == nil || *u.Assigned != 2 || !slices.Equal(u.Started, []Job{{Runner: "current", Workflow: ".github/workflows/ci.yml", Display: "test", Outcome: "unknown"}}) || !slices.Equal(u.Completed, []Job{{Runner: "previous", Outcome: "unknown"}}) {
		t.Fatal("missing acquisition, demand, or lifecycle event")
	}
	if u.JIT == nil {
		t.Fatal("missing session JIT callback")
	}
	if config, err := u.JIT(t.Context(), "current"); err != nil || config != "synthetic" {
		t.Fatal("incorrect session JIT callback")
	}
	if err := m.Scale(t.Context(), nil); err != nil || len(updates) != 2 || updates[1].Assigned != nil {
		t.Fatal("idle poll must publish a heartbeat and preserve demand")
	}
}

func TestCompletedJobPreservesBrokerWindowAndEvent(t *testing.T) {
	start := time.Now().Add(-time.Minute).UTC()
	end := start.Add(45 * time.Second)
	msg := scaleset.JobMessageBase{JobWorkflowRef: "example/repo/.github/workflows/test.yml@refs/pull/1/merge",
		JobDisplayName: "check", EventName: "pull_request", RunnerAssignTime: start, FinishTime: end}
	for raw, expected := range map[string]string{"Succeeded": "success", "Failed": "failure", "Canceled": "cancelled", "unexpected detail": "unknown"} {
		got := jobEvent("mmc-test", raw, msg)
		if got.Workflow != ".github/workflows/test.yml" || got.Display != "check" || got.Event != "pull_request" ||
			got.AssignedAt != start || got.FinishedAt != end || got.Outcome != expected {
			t.Fatal("incorrect lifecycle metadata")
		}
	}
}

type exchange func(*http.Request) (*http.Response, error)

func (f exchange) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestSessionConflictBacksOffAndPreservesScaleSet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var failures []time.Duration
		start := time.Now()
		handle := exchange(func(req *http.Request) (*http.Response, error) {
			var body string
			status := http.StatusOK
			switch req.Method + " " + req.URL.Path {
			case "POST /repos/example/project/actions/runners/registration-token":
				status, body = http.StatusCreated, `{"token":"test"}`
			case "POST /actions/runner-registration":
				encode := base64.RawURLEncoding.EncodeToString
				exp := encode([]byte(`{"exp":4102444800}`))
				body = `{"url":"https://example.invalid","token":"` + encode([]byte(`{"alg":"none"}`)) + "." + exp + `."}`
			case "GET /_apis/runtime/runnerscalesets":
				body = `{"count":1,"value":[{"id":1,"name":"test"}]}`
			case "POST /_apis/runtime/runnerscalesets/1/sessions":
				status, body = http.StatusConflict, `{"message":"private broker detail"}`
			default:
				t.Fatalf("unexpected API operation: %s %s", req.Method, req.URL.Path)
			}
			return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		})
		client, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL: "https://github.com/example/project", PersonalAccessToken: "synthetic",
		}, scaleset.WithHTTPClient(handle))
		if err != nil {
			t.Fatal(err)
		}
		err = listen(ctx, client, Config{Label: "test", MaxRunners: 1}, func(Update) {
			t.Fatal("conflicting listener received work")
		}, func(err error) {
			if err != ErrSessionConflict {
				t.Fatalf("unexpected conflict report: %v", err)
			}
			failures = append(failures, time.Since(start))
			if len(failures) == 3 {
				cancel()
			}
		})
		if !errors.Is(err, context.Canceled) || !slices.Equal(failures, []time.Duration{0, time.Second, 3 * time.Second}) {
			t.Fatalf("unexpected conflict retry times %v, return %v", failures, err)
		}
	})
}
