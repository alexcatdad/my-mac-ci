package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `{
	"registry": "registry.invalid/ci",
	"cap": {"cpus": 8, "memory": "16g"},
	"repos": {"example/project": {
		"linux": {"image": "ci/linux@sha256:0000000000000000000000000000000000000000000000000000000000000000", "cpus": 2, "memory": "512m"}
	}}
}`

func loadExample(t *testing.T, data string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestResourceLimits(t *testing.T) {
	cfg, err := loadExample(t, sample)
	if err != nil {
		t.Fatal(err)
	}
	label := cfg.Repos["example/project"]["linux"]
	if cfg.Cap != (Resources{CPUs: 8, Memory: 16 << 30}) || label.CPUs != 2 || label.Memory != 512<<20 || label.Max != 0 {
		t.Fatal("incorrect resource limits")
	}
	cfg, err = loadExample(t, strings.Replace(sample, `"cpus": 2`, `"max": 3, "cpus": 2`, 1))
	if err != nil || cfg.Repos["example/project"]["linux"].Max != 3 {
		t.Fatal("explicit label max was not loaded")
	}
}

func TestInvalidConfig(t *testing.T) {
	for _, tc := range []struct{ name, old, replacement string }{
		{"missing global memory", `"memory": "16g"`, `"memory": ""`},
		{"zero CPUs", `"cpus": 8`, `"cpus": 0`},
		{"overflow", `"16g"`, `"999999999999999999g"`},
		{"max zero", `"cpus": 2`, `"cpus": 2, "max": 0`},
		{"registry credentials", "registry.invalid", "sensitive@example.invalid"},
		{"unpinned image", "@sha256:", ":tag"},
		{"path traversal", "ci/linux", "../linux"},
		{"unknown field", `"cpus": 8`, `"sensitive": 8`},
		{"extra JSON value", "}\n}", "}\n} {}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadExample(t, strings.Replace(sample, tc.old, tc.replacement, 1))
			if err == nil || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "registry.invalid") {
				t.Fatal("expected a redacted configuration error")
			}
		})
	}
}
