package github

import (
	"context"
	"errors"

	"github.com/actions/scaleset"
)

type registrations interface {
	GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error)
	RemoveRunner(context.Context, int64) error
}

// RemoveRunner is the service-side assignment guard used by ARC scale-down.
// Only a confirmed removal allows the caller to stop its local VM.
func removeIdle(ctx context.Context, client registrations, set int, name string) (bool, error) {
	ref, err := client.GetRunnerByName(ctx, name)
	if err != nil || ref == nil || ref.ID <= 0 || ref.Name != name || ref.RunnerScaleSetID != set {
		return false, errUnavailable
	}
	err = client.RemoveRunner(ctx, int64(ref.ID))
	if errors.Is(err, scaleset.JobStillRunningError) {
		return false, nil
	}
	if err != nil {
		return false, errUnavailable
	}
	return true, nil
}
