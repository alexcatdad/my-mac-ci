package github

import (
	"context"
	"errors"
	"testing"

	"github.com/actions/scaleset"
)

type idleClient struct {
	ref       *scaleset.RunnerReference
	removeErr error
	id        int64
}

func (f *idleClient) GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error) {
	return f.ref, nil
}

func (f *idleClient) RemoveRunner(_ context.Context, id int64) error {
	f.id = id
	return f.removeErr
}

func TestRemoveIdleRequiresConfirmedRegistration(t *testing.T) {
	for _, tc := range []struct {
		name            string
		ref             *scaleset.RunnerReference
		response        error
		id              int64
		removed, failed bool
	}{
		{"missing", nil, nil, 0, false, true},
		{"other set", &scaleset.RunnerReference{ID: 7, Name: "mmc-a", RunnerScaleSetID: 2}, nil, 0, false, true},
		{"other runner", &scaleset.RunnerReference{ID: 7, Name: "mmc-b", RunnerScaleSetID: 1}, nil, 0, false, true},
		{"assigned", &scaleset.RunnerReference{ID: 7, Name: "mmc-a", RunnerScaleSetID: 1}, scaleset.JobStillRunningError, 7, false, false},
		{"ambiguous", &scaleset.RunnerReference{ID: 7, Name: "mmc-a", RunnerScaleSetID: 1}, errors.New("private detail"), 7, false, true},
		{"removed", &scaleset.RunnerReference{ID: 7, Name: "mmc-a", RunnerScaleSetID: 1}, nil, 7, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &idleClient{ref: tc.ref, removeErr: tc.response}
			removed, err := removeIdle(t.Context(), client, 1, "mmc-a")
			if client.id != tc.id || removed != tc.removed || errors.Is(err, errUnavailable) != tc.failed {
				t.Fatal("unexpected guarded removal")
			}
		})
	}
}
