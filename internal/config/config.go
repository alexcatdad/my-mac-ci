// Package config loads the local MMC configuration. Errors omit user-supplied
// values, including registry names and file paths.
package config

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/alexcatdad/my-mac-ci/internal/isolation"
)

type Config struct {
	Isolation *isolation.Options
	Registry  string
	Cap       Resources
	Repos     map[string]map[string]Label
}

type Resources struct {
	CPUs   int
	Memory int64 // bytes
}

type Label struct {
	Resources
	Image string
	Max   int // zero when omitted
}

var (
	repoName  = regexp.MustCompile(`^[a-zA-Z0-9-]+/[a-zA-Z0-9_.-]+$`)
	imagePath = regexp.MustCompile(`^[a-z0-9]+(?:(?:[.]|_{1,2}|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[.]|_{1,2}|-+)[a-z0-9]+)*)*$`)
	digest    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidImagePath accepts repository paths relative to the configured registry.
func ValidImagePath(path string) bool { return imagePath.MatchString(path) }

type diskResources struct {
	CPUs   int    `json:"cpus"`
	Memory string `json:"memory"`
}

func (r diskResources) resources() (Resources, error) {
	if r.CPUs < 1 || r.Memory == "" {
		return Resources{}, errors.New("positive CPU and memory limits are required")
	}
	amount := strings.ToLower(r.Memory)
	var multiplier int64 = 1
	for suffix, factor := range map[string]int64{"k": 1 << 10, "m": 1 << 20, "g": 1 << 30, "t": 1 << 40} {
		if strings.HasSuffix(amount, suffix) {
			amount = strings.TrimSuffix(amount, suffix)
			multiplier = factor
			break
		}
	}
	n, err := strconv.ParseUint(amount, 10, 63)
	if err != nil || n == 0 || n > uint64(math.MaxInt64/multiplier) {
		return Resources{}, errors.New("invalid memory limit")
	}
	return Resources{CPUs: r.CPUs, Memory: int64(n) * multiplier}, nil
}

func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("cannot locate home directory")
	}
	return filepath.Join(home, ".config", "mmc", "config.json"), nil
}

func Load(path string) (Config, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Config{}, errors.New("cannot open configuration file")
	}
	defer func() { _ = file.Close() }()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return Config{}, errors.New("configuration must be a regular file")
	}

	var raw struct {
		Isolation *isolation.Options `json:"isolation"`
		Registry  string             `json:"registry"`
		Cap       diskResources      `json:"cap"`
		Repos     map[string]map[string]struct {
			diskResources
			Image string `json:"image"`
			Max   *int   `json:"max"`
		} `json:"repos"`
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil || decoder.Decode(new(any)) != io.EOF {
		return Config{}, errors.New("invalid configuration JSON or unknown field")
	}
	registry, err := url.Parse("https://" + raw.Registry)
	if err != nil || registry.Hostname() == "" || registry.User != nil || registry.Host+registry.Path != raw.Registry ||
		(registry.Path != "" && !imagePath.MatchString(strings.TrimPrefix(registry.Path, "/"))) {
		return Config{}, errors.New("invalid registry location")
	}
	cap, err := raw.Cap.resources()
	if err != nil {
		return Config{}, err
	}
	if raw.Isolation != nil && !raw.Isolation.Valid() {
		return Config{}, errors.New("invalid isolation settings")
	}
	cfg := Config{Isolation: raw.Isolation, Registry: raw.Registry, Cap: cap, Repos: make(map[string]map[string]Label, len(raw.Repos))}
	for repo, labels := range raw.Repos {
		if !repoName.MatchString(repo) || strings.HasSuffix(repo, "/.") || strings.HasSuffix(repo, "/..") {
			return Config{}, errors.New("invalid repository name")
		}
		cfg.Repos[repo] = make(map[string]Label, len(labels))
		for name, label := range labels {
			if strings.TrimSpace(name) == "" || strings.TrimSpace(name) != name {
				return Config{}, errors.New("invalid runner label")
			}
			image, hash, found := strings.Cut(label.Image, "@sha256:")
			if !found || !ValidImagePath(image) || !digest.MatchString(hash) {
				return Config{}, errors.New("runner image must be a repository path pinned by SHA-256 digest")
			}
			resources, err := label.resources()
			if err != nil {
				return Config{}, err
			}
			entry := Label{Resources: resources, Image: label.Image}
			if label.Max != nil {
				if *label.Max < 1 {
					return Config{}, errors.New("label max must be positive")
				}
				entry.Max = *label.Max
			}
			cfg.Repos[repo][name] = entry
		}
	}
	return cfg, nil
}
