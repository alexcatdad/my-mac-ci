// Command mmc manages runner images and reviews local job resource usage.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/alexcatdad/my-mac-ci/internal/config"
	"github.com/alexcatdad/my-mac-ci/internal/runnerimage"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) >= 2 && args[0] == "sizing" && args[1] == "review" {
		return sizingReview(args[2:], out)
	}
	if len(args) < 2 || args[0] != "image" || args[1] != "build" {
		return errors.New("usage: mmc sizing review | mmc image build [-base] [-name NAME] [CONTEXT]")
	}
	flags := flag.NewFlagSet("mmc image build", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	base := flags.Bool("base", false, "build the shared MMC base from the MMC checkout")
	name := flags.String("name", "", "image repository path relative to the configured registry")
	if err := flags.Parse(args[2:]); err != nil || flags.NArg() > 1 || (*base && flags.NArg() != 0) {
		return errors.New("usage: mmc image build [-base] [-name NAME] [CONTEXT]")
	}
	checkout, err := os.Getwd()
	if err != nil {
		return errors.New("cannot locate checkout")
	}
	dir := checkout
	if flags.NArg() == 1 {
		dir, err = filepath.Abs(flags.Arg(0))
		if err != nil {
			return errors.New("invalid build context")
		}
	}
	if *base {
		dir = filepath.Join(checkout, "trial", "image")
	}
	if *name == "" {
		*name = filepath.Base(checkout)
		if *base {
			*name = "mmc-runner"
		}
	}
	if !config.ValidImagePath(*name) {
		return errors.New("invalid image repository name")
	}
	path, err := config.DefaultPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	ref, err := build(ctx, checkout, dir, cfg.Registry, *name, *base)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, ref)
	return err
}

func command(ctx context.Context, timeout time.Duration, dir string, stdout io.Writer, program string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	cmd.Stdout = stdout
	return cmd.Run()
}

var imageDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func build(ctx context.Context, checkout, dir, registry, name string, base bool) (ref string, err error) {
	// Apple container uses one builder per user. Serialize MMC build/stop pairs.
	cache, e := os.UserCacheDir()
	if e != nil {
		return "", errors.New("cannot locate build lock")
	}
	cache = filepath.Join(cache, "my-mac-ci")
	if os.MkdirAll(cache, 0700) != nil {
		return "", errors.New("cannot create build lock")
	}
	lock, e := os.OpenFile(filepath.Join(cache, "image-build.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return "", errors.New("cannot open build lock")
	}
	defer func() { _ = lock.Close() }()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return "", errors.New("another MMC image build holds the builder lock")
	}
	if base {
		if e = command(ctx, 5*time.Minute, checkout, nil, "task", "build:jit-files"); e != nil {
			return "", errors.New("could not stage the shared runner decoder")
		}
	}
	stage, e := os.MkdirTemp("", "mmc-image-")
	if e != nil {
		return "", errors.New("cannot stage build context")
	}
	defer func() {
		if os.RemoveAll(stage) != nil {
			err = errors.Join(err, errors.New("could not remove staged context"))
		}
	}()
	sum, e := stageContext(ctx, dir, stage, base)
	if e != nil {
		return "", e
	}
	local := "mmc-build-" + strings.ToLower(rand.Text()) + ":local"
	defer func() {
		clean := context.WithoutCancel(ctx)
		if e := command(clean, time.Minute, "", nil, "container", "builder", "stop"); e != nil {
			err = errors.Join(err, errors.New("could not stop the builder"))
		}
	}()
	defer func() {
		if e := command(context.WithoutCancel(ctx), time.Minute, "", nil, "container", "image", "delete", "--force", local); e != nil {
			err = errors.Join(err, errors.New("could not remove temporary local image tag"))
		}
	}()
	fmt.Fprintln(os.Stderr, "Building Linux arm64 runner image…")
	if e = command(ctx, 45*time.Minute, "", nil, "container", "build", "--progress", "plain", "--platform", "linux/arm64", "--build-arg", "MMC_REGISTRY="+registry, "-t", local, stage); e != nil {
		return "", errors.New("runner image build failed")
	}
	var raw bytes.Buffer
	e = command(ctx, time.Minute, "", &raw, "container", "image", "inspect", local)
	if e != nil {
		return "", errors.New("cannot inspect built image")
	}
	digest, version, e := runnerimage.Metadata(raw.Bytes())
	if e != nil || !imageDigest.MatchString(digest) {
		return "", errors.New("expected a Linux arm64 image with a valid digest and runner version")
	}
	// The staged Dockerfile includes the pinned parent digest. Include the
	// resolved runner version even when its label was inherited from that parent.
	key := sha256.Sum256(append(sum[:], version...))
	tag := fmt.Sprintf("%s-%x", version, key[:8])
	fmt.Fprintln(os.Stderr, "Checking registry tag and digest…")
	return publish(ctx, registry, name, tag, local, digest)
}
