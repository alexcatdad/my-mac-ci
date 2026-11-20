// Package runnerimage reads Linux arm64 runner image metadata.
package runnerimage

import (
	"context"
	"encoding/json"
	"errors"
	"go/version"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type Status struct {
	Version, Latest string
	Outdated        bool
}

var numericVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// Metadata extracts the local digest and embedded runner version using Apple's inspect format.
func Metadata(data []byte) (digest, runner string, err error) {
	var images []struct {
		Configuration struct{ Descriptor struct{ Digest string } }
		Variants      []struct {
			Config struct {
				Architecture, OS string
				Config           struct{ Labels map[string]string }
			}
		}
	}
	if json.Unmarshal(data, &images) == nil && len(images) == 1 {
		for _, v := range images[0].Variants {
			n := v.Config.Config.Labels["mmc.runner.version"]
			if v.Config.OS == "linux" && v.Config.Architecture == "arm64" && numericVersion.MatchString(n) {
				return images[0].Configuration.Descriptor.Digest, n, nil
			}
		}
	}
	return "", "", errors.New("expected a Linux arm64 image with a runner version label")
}

// Check observes cached images only. A failed release lookup leaves Latest unknown.
func Check(ctx context.Context, refs []string) map[string]Status {
	results := make(map[string]Status, len(refs))
	if len(refs) == 0 {
		return results
	}
	latest := latestRelease(ctx)
	for _, ref := range refs {
		if ctx.Err() != nil {
			break
		}
		if _, ok := results[ref]; ok {
			continue
		}
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		data, err := exec.CommandContext(bounded, "container", "image", "inspect", ref).Output()
		cancel()
		s := Status{Latest: latest}
		if err == nil {
			digest, v, err := Metadata(data)
			if err == nil && strings.HasSuffix(ref, "@"+digest) {
				s.Version = v
				s.Outdated = latest != "" && version.Compare("go"+v, "go"+latest) < 0
			}
		}
		results[ref] = s
	}
	return results
}

func latestRelease(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/actions/runner/releases/latest", nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var release struct {
		Tag string `json:"tag_name"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&release) != nil {
		return ""
	}
	v := strings.TrimPrefix(release.Tag, "v")
	if !numericVersion.MatchString(v) {
		return ""
	}
	return v
}
