package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/alexcatdad/my-mac-ci/internal/config"
	"github.com/alexcatdad/my-mac-ci/internal/dispatch"
	"github.com/alexcatdad/my-mac-ci/internal/github"
	"github.com/alexcatdad/my-mac-ci/internal/isolation"
)

func helperDirectory() (string, error) {
	account, err := user.Current()
	if err != nil || account.HomeDir == "" {
		return "", errors.New("cannot resolve the current user’s application support directory")
	}
	return filepath.Join(account.HomeDir, "Library", "Application Support", "my-mac-ci"), nil
}

func runHelper(args []string) int {
	dir, err := helperDirectory()
	if err != nil {
		return helperExitCode(err)
	}
	return runHelperAt(args, dir)
}

func runHelperAt(args []string, dir string) int {
	flags := flag.NewFlagSet("mmc-helper run", flag.ContinueOnError)
	path := flags.String("config", "", "path to MMC JSON configuration (default ~/.config/mmc/config.json)")
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: mmc-helper run [-config path]")
		fmt.Fprintln(os.Stderr, "Supply the App private key on stdin and set MMC_GITHUB_APP_CLIENT_ID and MMC_GITHUB_APP_INSTALLATION_ID.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return 2
	}
	var err error
	if *path == "" {
		*path, err = config.DefaultPath()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return helperExitCode(supervise(ctx, dir, *path, os.Stdin))
}

func helperExitCode(err error) int {
	if errors.Is(err, context.Canceled) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, errAlreadyRunning) {
			return 3
		}
		return 1
	}
	return 0
}

func supervise(ctx context.Context, dir, path string, input *os.File) error {
	lock, err := lockHelper(dir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	cfg, err := dispatchConfig(path)
	if err != nil {
		return err
	}
	auth := github.Config{ClientID: os.Getenv("MMC_GITHUB_APP_CLIENT_ID")}
	auth.InstallationID, err = strconv.ParseInt(os.Getenv("MMC_GITHUB_APP_INSTALLATION_ID"), 10, 64)
	if err != nil || auth.InstallationID <= 0 || auth.ClientID == "" {
		return errors.New("run requires MMC_GITHUB_APP_CLIENT_ID and MMC_GITHUB_APP_INSTALLATION_ID")
	}
	info, err := input.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice != 0 {
		return errors.New("supply the App private key through redirected stdin")
	}
	// A signal must also release the instance lock if the input pipe stalls.
	stop := context.AfterFunc(ctx, func() { _ = input.Close() })
	defer stop()
	key, err := io.ReadAll(io.LimitReader(input, maxKeyBytes+1))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || len(key) > maxKeyBytes || !validPrivateKey(key) {
		return errors.New("stdin must contain one RSA App private key in PEM format")
	}
	auth.PrivateKey = string(key)
	clear(key)
	return startDispatch(ctx, cfg, auth)
}

func dispatchConfig(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err == nil && cfg.Isolation == nil {
		err = errors.New("dispatch requires isolation settings in the configuration")
	}
	return cfg, err
}

func startDispatch(ctx context.Context, cfg config.Config, auth github.Config) error {
	return dispatch.Serve(ctx, cfg, auth, isolation.NewMonitor(*cfg.Isolation))
}

const maxKeyBytes = 64 << 10

func validPrivateKey(b []byte) bool {
	block, rest := pem.Decode(b)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return false
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		_, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		return err == nil
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		_, rsaKey := key.(*rsa.PrivateKey)
		return err == nil && rsaKey
	}
	return false
}

var errAlreadyRunning = errors.New("another MMC helper is already running")

func privateDirectory(dir string) bool {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false
	}
	d, err := os.Lstat(dir)
	return err == nil && d.IsDir() && d.Mode().Perm()&0077 == 0 && d.Sys().(*syscall.Stat_t).Uid == uint32(os.Getuid())
}

func lockHelper(dir string) (*os.File, error) {
	unavailable := errors.New("cannot acquire the MMC instance lock")
	if !privateDirectory(dir) {
		return nil, unavailable
	}
	// All configurations share one inode. Closing the fd (including a crash)
	// releases flock; never unlink or replace the lock file.
	file, err := os.OpenFile(filepath.Join(dir, "helper.lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, unavailable
	}
	info, err := file.Stat()
	if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0077 == 0 && info.Sys().(*syscall.Stat_t).Uid == uint32(os.Getuid()) {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file, nil
		}
	}
	_ = file.Close()
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, errAlreadyRunning
	}
	return nil, unavailable
}
