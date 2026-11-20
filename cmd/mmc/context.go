package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Stage exactly the bytes used by both the tag calculation and the builder.
// Git metadata and local ignored files stay out of project images.
func stageContext(ctx context.Context, dir, stage string, base bool) ([32]byte, error) {
	var sum [32]byte
	var raw bytes.Buffer
	err := command(ctx, time.Minute, dir, &raw, "git", "ls-files", "--cached", "--others", "--exclude-standard", "-z", "--", ".")
	if err != nil {
		return sum, errors.New("build context must be in a Git checkout")
	}
	files := strings.Split(string(bytes.TrimSuffix(raw.Bytes(), []byte{0})), "\x00")
	if base {
		files = append(files, "mmc-jit-files")
	}
	slices.Sort(files)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return sum, errors.New("cannot open build context")
	}
	defer func() { _ = root.Close() }()
	h := sha256.New()
	for _, name := range slices.Compact(files) {
		if ctx.Err() != nil {
			return sum, ctx.Err()
		}
		if !filepath.IsLocal(name) {
			return sum, errors.New("invalid context entry")
		}
		info, err := root.Lstat(name)
		if os.IsNotExist(err) {
			continue
		} // tracked file removed in the working tree
		if err != nil {
			return sum, errors.New("cannot inspect context entry")
		}
		dest := filepath.Join(stage, name)
		if os.MkdirAll(filepath.Dir(dest), 0755) != nil {
			return sum, errors.New("cannot stage context directory")
		}
		_, _ = fmt.Fprintf(h, "%d:%s:%o:", len(name), name, info.Mode())
		fileHash := sha256.New()
		switch {
		case info.Mode().IsRegular():
			if err := copyFile(root, name, dest, info.Mode().Perm(), fileHash); err != nil {
				return sum, errors.New("cannot stage context file")
			}
		case info.Mode()&os.ModeSymlink != 0:
			target, e := root.Readlink(name)
			if e != nil || os.Symlink(target, dest) != nil {
				return sum, errors.New("cannot stage context symlink")
			}
			_, _ = io.WriteString(fileHash, target)
		default:
			return sum, errors.New("unsupported build context entry")
		}
		h.Write(fileHash.Sum(nil))
	}
	dockerfile, err := os.Lstat(filepath.Join(stage, "Dockerfile"))
	if err != nil || !dockerfile.Mode().IsRegular() {
		return sum, errors.New("build context needs a Dockerfile")
	}
	contents, err := os.ReadFile(filepath.Join(stage, "Dockerfile"))
	if err != nil {
		return sum, errors.New("cannot read Dockerfile")
	}
	pin, err := parentDigest(contents)
	if err != nil {
		return sum, err
	}
	_, _ = io.WriteString(h, pin)
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

func copyFile(root *os.Root, name, dest string, mode os.FileMode, h io.Writer) error {
	src, err := root.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(io.MultiWriter(dst, h), src)
	return errors.Join(err, dst.Chmod(mode), dst.Close())
}

// Resolve the final image’s parent digest, including stage
// aliases and ARG defaults declared before the first stage.
func parentDigest(dockerfile []byte) (string, error) {
	variables := map[string]string{"MMC_REGISTRY": "registry.invalid"}
	stages := map[string]string{}
	var parent string
	started := false
	source := strings.ReplaceAll(string(dockerfile), "\r\n", "\n")
	source = strings.ReplaceAll(source, "\\\n", " ")
	for line := range strings.SplitSeq(source, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "ARG":
			if !started {
				key, val, ok := strings.Cut(fields[1], "=")
				if ok && key != "MMC_REGISTRY" {
					variables[key] = os.Expand(strings.Trim(val, "\"'"), func(k string) string { return variables[k] })
				}
			}
		case "FROM":
			started = true
			args := fields[1:]
			if strings.HasPrefix(args[0], "--platform=") {
				args = args[1:]
			}
			alias := len(args) == 3 && strings.EqualFold(args[1], "AS")
			if len(args) != 1 && !alias {
				return "", errors.New("invalid Dockerfile FROM instruction")
			}
			base := os.Expand(args[0], func(k string) string { return variables[k] })
			parent = base
			if original, ok := stages[strings.ToLower(base)]; ok {
				parent = original
			}
			if alias {
				stages[strings.ToLower(args[2])] = parent
			}
		}
	}
	_, digest, ok := strings.Cut(parent, "@")
	if !ok || !imageDigest.MatchString(digest) {
		return "", errors.New("pin the runner Dockerfile parent with @sha256:<digest>")
	}
	return digest, nil
}
