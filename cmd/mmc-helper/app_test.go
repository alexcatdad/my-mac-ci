package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func socketDirectory(t *testing.T) string {
	t.Helper()
	// Darwin limits the full sockaddr_un path to 103 bytes.
	dir, err := os.MkdirTemp("/tmp", "mmc-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func waitStatusSocket(t *testing.T, path string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
		if err == nil {
			return conn
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper did not open its status socket")
	return nil
}

func TestAppModeStatusAndSupervisorExit(t *testing.T) {
	socketDir := socketDirectory(t)
	support := filepath.Join(t.TempDir(), "support")
	path := filepath.Join(socketDir, "helper.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: path})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = write.Close(); _ = read.Close() })
	done := make(chan int, 1)
	go func() { done <- appModeAt([]string{"--socket-dir", socketDir}, support, read, runAppDispatch) }()
	conn := waitStatusSocket(t, path)
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "status\n"); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil || string(got) != "{\"state\":\"awaiting_credentials\"}\n" {
		t.Fatal("unexpected helper status")
	}
	if info, err := os.Lstat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("expected owner-only socket permissions")
	}
	if lock, err := lockHelper(support); !errors.Is(err, errAlreadyRunning) {
		if lock != nil {
			_ = lock.Close()
		}
		t.Fatal("app mode must share the standalone instance lock")
	}

	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("app mode exited with %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("helper survived supervisor pipe EOF")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale socket after helper exit")
	}
}

func TestAppModePreservesOccupiedSocket(t *testing.T) {
	dir := socketDirectory(t)
	path := filepath.Join(dir, "helper.sock")
	active, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = active.Close() }()
	if err := serveAppSocket(t.Context(), filepath.Join(t.TempDir(), "support"), dir, runAppDispatch); !errors.Is(err, errAlreadyRunning) {
		t.Fatal("expected active socket conflict")
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal("existing listener no longer reachable")
	}
	_ = conn.Close()
}

func TestAppModePreservesReplacementOnShutdown(t *testing.T) {
	dir := socketDirectory(t)
	path := filepath.Join(dir, "helper.sock")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	support := filepath.Join(t.TempDir(), "support")
	go func() { done <- serveAppSocket(ctx, support, dir, runAppDispatch) }()
	first := waitStatusSocket(t, path)
	_ = first.Close()
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	replacement, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = replacement.Close() }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("helper did not stop")
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal("helper removed another listener during shutdown")
	}
	_ = conn.Close()
}

func TestAppModeRejectsExposedDirectoryAndExistingFiles(t *testing.T) {
	support := filepath.Join(t.TempDir(), "support")
	dir := socketDirectory(t)
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := serveAppSocket(t.Context(), support, dir, runAppDispatch); !errors.Is(err, errSocket) {
		t.Fatal("expected an owner-only directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "helper.sock")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := serveAppSocket(t.Context(), support, dir, runAppDispatch); !errors.Is(err, errSocket) {
		t.Fatal("expected occupied path to be rejected")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "fixture" {
		t.Fatal("existing file was changed")
	}
}
