package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/alexcatdad/my-mac-ci/internal/config"
	"github.com/alexcatdad/my-mac-ci/internal/dispatch"
	"github.com/alexcatdad/my-mac-ci/internal/github"
)

var errSocket = errors.New("the private MMC helper socket is unavailable")

func appMode(args []string) int {
	dir, err := helperDirectory()
	if err != nil {
		return helperExitCode(err)
	}
	return appModeAt(args, dir, os.Stdin, runAppDispatch)
}

func appModeAt(args []string, supportDir string, lifetime *os.File, dispatch appDispatchFunc) int {
	flags := flag.NewFlagSet("mmc-helper --app-mode", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("socket-dir", supportDir, "private socket directory")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || !filepath.IsAbs(*dir) {
		fmt.Fprintln(os.Stderr, "usage: mmc-helper --app-mode [--socket-dir path]")
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if os.Getppid() == 1 {
		return 0
	}
	info, err := lifetime.Stat()
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		return helperExitCode(errors.New("app mode requires a supervisor lifetime pipe on stdin"))
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// The app owns the only write end. Its exit closes the pipe even if the
	// helper has not yet bound its socket. Credentials never use this pipe.
	go func() {
		_, _ = io.Copy(io.Discard, lifetime)
		cancel()
	}()
	return helperExitCode(serveAppSocket(ctx, supportDir, *dir, dispatch))
}

type appDispatchFunc func(context.Context, github.Config) error

func runAppDispatch(ctx context.Context, auth github.Config) error {
	cfg, err := appConfig()
	if err != nil {
		return err
	}
	if err := ensureContainerRuntime(ctx); err != nil {
		return err
	}
	return startDispatch(ctx, cfg, auth)
}

// Login can start MMC before Apple container's user-session API is running.
func ensureContainerRuntime(ctx context.Context) error {
	call := func(timeout time.Duration, args ...string) error {
		bounded, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return exec.CommandContext(bounded, "container", args...).Run()
	}
	if call(5*time.Second, "system", "status") == nil {
		return nil
	}
	if err := call(40*time.Second, "system", "start", "--disable-kernel-install", "--timeout", "30"); err != nil {
		return errors.New("cannot start the container runtime")
	}
	return nil
}

func appConfig() (config.Config, error) {
	account, err := user.Current()
	if err != nil || !filepath.IsAbs(account.HomeDir) {
		return config.Config{}, errors.New("cannot resolve the current user’s home directory")
	}
	return dispatchConfig(filepath.Join(account.HomeDir, ".config", "mmc", "config.json"))
}

func serveAppSocket(ctx context.Context, supportDir, socketDir string, dispatch appDispatchFunc) error {
	lock, err := lockHelper(supportDir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if !privateDirectory(socketDir) {
		return errSocket
	}
	path := filepath.Join(socketDir, "helper.sock")
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
			return errSocket
		}
		// The instance lock serializes MMC launches. A listener outside MMC
		// might also occupy this name, so require a refused connect before
		// removing even an owner-matched socket.
		dial := net.Dialer{Timeout: time.Second}
		if conn, err := dial.DialContext(ctx, "unix", path); err == nil {
			_ = conn.Close()
			return errAlreadyRunning
		} else if !errors.Is(err, syscall.ECONNREFUSED) || !removeSameFile(path, info) {
			return errSocket
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errSocket
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return errSocket
	}
	listener.SetUnlinkOnClose(false)
	defer func() { _ = listener.Close() }()
	bound, err := os.Lstat(path)
	if err != nil || bound.Mode()&os.ModeSocket == 0 || bound.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return errSocket
	}
	defer removeSameFile(path, bound)
	if err := os.Chmod(path, 0600); err != nil {
		return errSocket
	}
	ctx, cancel := context.WithCancel(ctx)
	session := appSession{state: "awaiting_credentials", parent: os.Getppid(), dispatch: dispatch}
	// Join dispatch before releasing the per-user instance lock.
	defer func() {
		cancel()
		if session.done != nil {
			<-session.done
		}
	}()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errSocket
		}
		session.handle(ctx, conn)
	}
}

func removeSameFile(path string, original os.FileInfo) bool {
	info, err := os.Lstat(path)
	return err == nil && os.SameFile(info, original) && os.Remove(path) == nil
}

// Socket requests are served serially. Only completion crosses goroutines.
type appSession struct {
	quit     *quitCheck
	health   *dispatch.Health
	state    string
	parent   int
	dispatch appDispatchFunc
	done     chan struct{}
}

func (s *appSession) handle(ctx context.Context, conn *net.UnixConn) {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer func() { _ = conn.Close() }()
	if conn.SetDeadline(time.Now().Add(2*time.Second)) != nil {
		return
	}
	reader := bufio.NewReaderSize(conn, 16)
	command, err := reader.ReadSlice('\n')
	if err != nil {
		return
	}
	select {
	case <-s.done:
		s.done = nil
		s.state = "dispatch_failed"
	default:
	}
	reply := s.state
	var reloadError string
	switch string(command) {
	case "quit-prepare\n", "quit-stop\n", "quit-resume\n":
		if parentPeer(conn, s.parent) {
			_ = json.NewEncoder(conn).Encode(map[string]string{
				"state": s.state, "quit": s.quitCommand(ctx, strings.TrimSpace(string(command))),
			})
		}
		return
	case "reload\n":
		if !parentPeer(conn, s.parent) {
			return
		}
		reloadError = "Reload requires an active dispatch session."
		if s.done != nil && s.quit == nil && s.health.Controller() != nil {
			cfg, err := appConfig()
			if err != nil {
				reloadError = err.Error()
			} else {
				bounded, cancel := context.WithTimeout(ctx, time.Second)
				err = s.health.Reload(bounded, cfg)
				cancel()
				if err == nil {
					reloadError = ""
				} else {
					reloadError = "The helper could not apply the configuration."
				}
			}
		}
	case "status\n":
	case "auth\n":
		if !parentPeer(conn, s.parent) {
			reply = "invalid_credentials"
		} else {
			reply = s.authenticate(ctx, reader)
		}
	default:
		return
	}
	var health *dispatch.Snapshot
	if s.health != nil && parentPeer(conn, s.parent) {
		health = s.health.Snapshot()
	}
	_ = json.NewEncoder(conn).Encode(struct {
		State       string             `json:"state"`
		Health      *dispatch.Snapshot `json:"health,omitempty"`
		ReloadError string             `json:"reloadError,omitempty"`
	}{reply, health, reloadError})
}

func parentPeer(conn *net.UnixConn, parent int) bool {
	if runtime.GOOS != "darwin" || parent <= 1 || os.Getppid() != parent {
		return false
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	// Darwin <sys/un.h>: SOL_LOCAL and LOCAL_PEERPID.
	const solLocal, localPeerPID = 0, 0x002
	allowed := false
	err = raw.Control(func(fd uintptr) {
		pid, err := syscall.GetsockoptInt(int(fd), solLocal, localPeerPID)
		allowed = err == nil && pid == parent
	})
	return err == nil && allowed
}

const maxAuthLine = 128 << 10

func (s *appSession) authenticate(ctx context.Context, input io.Reader) string {
	// Each credential frame must end with LF and fit, including LF, in the
	// bounded buffer. The credential value itself is never echoed or logged.
	line, err := bufio.NewReaderSize(input, maxAuthLine).ReadSlice('\n')
	defer clear(line)
	var request struct {
		ClientID       string `json:"clientID"`
		InstallationID int64  `json:"installationID"`
		PrivateKey     string `json:"privateKey"`
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err != nil || decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF ||
		strings.TrimSpace(request.ClientID) == "" || request.InstallationID <= 0 ||
		len(request.PrivateKey) > maxKeyBytes || !validPrivateKey([]byte(request.PrivateKey)) {
		if s.done == nil {
			s.state = "invalid_credentials"
		}
		return "invalid_credentials"
	}
	if s.done != nil || s.quit != nil {
		return s.state
	}
	auth := github.Config{ClientID: request.ClientID, InstallationID: request.InstallationID, PrivateKey: request.PrivateKey}
	done := make(chan struct{})
	s.done = done
	s.health = new(dispatch.Health)
	ctx = dispatch.WithHealth(ctx, s.health)
	s.state = "dispatching"
	go func() {
		defer close(done)
		_ = s.dispatch(ctx, auth)
	}()
	return s.state
}
