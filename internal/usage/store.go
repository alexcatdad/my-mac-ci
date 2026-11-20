package usage

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var (
	errStore = errors.New("local usage history unavailable")
	sumName  = regexp.MustCompile(`^[a-f0-9]{64}$`)
	digest   = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

func DefaultDir() (string, error) {
	u, err := user.Current()
	if err != nil || !filepath.IsAbs(u.HomeDir) {
		return "", errStore
	}
	return filepath.Join(u.HomeDir, "Library", "Application Support", "my-mac-ci", "usage"), nil
}

func private(info os.FileInfo, perm os.FileMode) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Getuid()) && info.Mode().Perm() == perm
}

func directory(path string, create bool) (*os.Root, error) {
	if !filepath.IsAbs(path) {
		return nil, errStore
	}
	if create && os.MkdirAll(path, 0700) != nil {
		return nil, errStore
	}
	info, err := os.Lstat(path)
	if !create && errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.IsDir() || !private(info, 0700) {
		return nil, errStore
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, errStore
	}
	check, err := root.Stat(".")
	if err != nil || !os.SameFile(info, check) {
		_ = root.Close()
		return nil, errStore
	}
	return root, nil
}

func read(root *os.Root, name string) (Record, bool, error) {
	var r Record
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return r, false, nil
	}
	if err != nil {
		return r, false, errStore
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !private(info, 0600) || info.Size() > 16384 {
		return r, false, errStore
	}
	b, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(b) > 16384 || json.Unmarshal(b, &r) != nil || !r.valid() {
		return Record{}, false, errStore
	}
	return r, true, nil
}

func (r Record) valid() bool {
	switch r.Outcome {
	case "unknown", "success", "failure", "cancelled":
	default:
		return false
	}
	return (r.Event == "" || r.Event == event(r.Event)) && sumName.MatchString(r.Group) && sumName.MatchString(r.LabelGroup) &&
		(r.ImageDigest == "" || digest.MatchString(r.ImageDigest)) &&
		r.CPUs >= 0 && r.MemoryBytes >= 0 && r.Samples >= 0 && r.Missed >= 0 &&
		r.AverageCores >= 0 && r.PeakCores >= r.AverageCores && r.CoveredMillis >= 0 &&
		(r.Samples == 0 || !r.SampledFrom.IsZero() && !r.SampledThrough.Before(r.SampledFrom))
}

func event(s string) string {
	switch s {
	case "push", "pull_request", "pull_request_target", "merge_group", "schedule", "workflow_dispatch", "workflow_call", "workflow_run", "repository_dispatch", "release", "deployment", "deployment_status", "issue_comment", "issues":
		return s
	default:
		return "unknown"
	}
}

// Read sees whole old or new summaries even when the helper is writing.
func Read(dir string) ([]Record, error) {
	root, err := directory(dir, false)
	if err != nil || root == nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	d, err := root.Open(".")
	if err != nil {
		return nil, errStore
	}
	defer func() { _ = d.Close() }()
	var records []Record
	for {
		names, err := d.Readdirnames(128)
		for _, n := range names {
			if !strings.HasSuffix(n, ".json") || !sumName.MatchString(strings.TrimSuffix(n, ".json")) {
				continue
			}
			r, ok, err := read(root, n)
			if err != nil {
				return nil, err
			}
			if ok {
				records = append(records, r)
			}
		}
		if err == io.EOF {
			return records, nil
		}
		if err != nil {
			return nil, errStore
		}
	}
}

func update(dir, runner string, j Job, fresh *Record) error {
	if !runnerID.MatchString(runner) || j.Repo == "" || j.Label == "" {
		return errStore
	}
	root, err := directory(dir, fresh != nil)
	if err != nil || root == nil {
		return err
	}
	defer func() { _ = root.Close() }()
	// Lock the directory, including across helper restarts; readers use atomic
	// replacement and never have to wait for the writer.
	lock, err := root.Open(".")
	if err != nil {
		return errStore
	}
	defer func() { _ = lock.Close() }()
	until := time.Now().Add(time.Second)
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(until) {
			return errStore
		}
		time.Sleep(10 * time.Millisecond)
	}
	label := LabelGroup(j.Repo, j.Label)
	key := hash(label, runner) + ".json"
	r, exists, err := read(root, key)
	if err != nil || (!exists && fresh == nil) {
		return err
	}
	if !exists {
		r = *fresh
	} else if fresh != nil {
		merge(&r, *fresh)
	}
	r.LabelGroup = label
	if r.Event == "" || r.Event == "unknown" {
		r.Event = event(j.Event)
	}
	if !r.WorkloadKnown {
		r.WorkloadKnown = j.Workflow != "" && j.Job != ""
		r.Group = WorkloadGroup(j.Repo, j.Label, j.Workflow, j.Job)
	}
	if r.CPUs == 0 && j.CPUs > 0 {
		r.CPUs = j.CPUs
	}
	if r.MemoryBytes == 0 && j.MemoryBytes > 0 {
		r.MemoryBytes = j.MemoryBytes
	}
	if r.ImageDigest == "" && digest.MatchString(j.ImageDigest) {
		r.ImageDigest = j.ImageDigest
	}
	if r.Outcome == "" || r.Outcome == "unknown" {
		switch j.Outcome {
		case "success", "failure", "cancelled":
			r.Outcome = j.Outcome
		default:
			r.Outcome = "unknown"
		}
	}
	if !j.StartedAt.IsZero() {
		r.StartedAt = j.StartedAt.UTC()
	}
	if !j.FinishedAt.IsZero() {
		r.FinishedAt = j.FinishedAt.UTC()
	}
	tmp := ".pending-" + rand.Text()
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errStore
	}
	defer func() { _ = root.Remove(tmp) }()
	writeErr := json.NewEncoder(f).Encode(&r)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil || root.Rename(tmp, key) != nil {
		return errStore
	}
	return nil
}

// Recovery can observe another window of the same runner. An already stored
// window is never counted twice when a lifecycle event is redelivered.
func merge(r *Record, s Record) {
	if s.Samples == 0 {
		return
	}
	if r.Samples > 0 && !s.SampledFrom.After(r.SampledThrough) && !s.SampledThrough.Before(r.SampledFrom) {
		return
	}
	total := r.Samples + s.Samples
	r.AverageCores += (s.AverageCores - r.AverageCores) * float64(s.Samples) / float64(total)
	if r.Samples == 0 || s.SampledFrom.Before(r.SampledFrom) {
		r.SampledFrom = s.SampledFrom
	}
	if s.SampledThrough.After(r.SampledThrough) {
		r.SampledThrough = s.SampledThrough
	}
	r.Samples = total
	r.Missed += s.Missed
	r.CoveredMillis += s.CoveredMillis
	r.PeakCores = max(r.PeakCores, s.PeakCores)
	r.PeakMemoryBytes = max(r.PeakMemoryBytes, s.PeakMemoryBytes)
	if s.FinishedAt.After(r.FinishedAt) {
		r.FinishedAt = s.FinishedAt
	}
}
