package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alexcatdad/my-mac-ci/internal/github"
)

// TestAppHandoffFixture is also used by the Swift socket integration test.
// Only constant lifecycle markers are written to the caller's private test
// directory. The credential stays in memory and dispatch has no network.
func TestAppHandoffFixture(t *testing.T) {
	dir := os.Getenv("MMC_TEST_SOCKET_DIR")
	if dir == "" {
		t.Skip("subprocess fixture")
	}
	fake := func(ctx context.Context, auth github.Config) error {
		if auth.ClientID == "" || auth.InstallationID <= 0 || !validPrivateKey([]byte(auth.PrivateKey)) {
			return errors.New("invalid synthetic handoff")
		}
		f, err := os.OpenFile(filepath.Join(dir, "dispatch.started"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		_, err = io.WriteString(f, "started\n")
		if err = errors.Join(err, f.Close()); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(dir, "fail-dispatch")); err == nil {
			return errors.New("synthetic startup failure")
		}
		<-ctx.Done()
		return os.WriteFile(filepath.Join(dir, "dispatch.stopped"), []byte("stopped\n"), 0600)
	}
	if code := appModeAt(nil, dir, os.Stdin, fake); code != 0 {
		t.Fatalf("fixture exited with code %d", code)
	}
}

func socketReply(t *testing.T, path, frame string) string {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal("socket connection failed")
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, frame); err != nil {
		t.Fatal("socket request failed")
	}
	line, err := bufio.NewReader(io.LimitReader(conn, 128)).ReadBytes('\n')
	var reply struct{ State string }
	if err != nil || json.Unmarshal(line, &reply) != nil {
		t.Fatal("invalid socket reply")
	}
	return reply.State
}

func syntheticAuth(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	frame, err := json.Marshal(map[string]any{"clientID": "synthetic", "installationID": 1, "privateKey": string(privateKey)})
	if err != nil {
		t.Fatal("cannot encode test credential")
	}
	return "auth\n" + string(frame) + "\n"
}

func TestAppSocketCredentialHandoff(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS parent peer authentication")
	}
	dir := socketDirectory(t)
	path := filepath.Join(dir, "helper.sock")
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAppHandoffFixture$")
	cmd.Env = []string{"MMC_TEST_SOCKET_DIR=" + dir}
	lifeline, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lifeline.Close() }()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	first := waitStatusSocket(t, path)
	_ = first.Close()
	auth := syntheticAuth(t)
	for _, test := range []struct{ frame, state string }{
		{"status\n", "awaiting_credentials"},
		{"auth\n{}\n", "invalid_credentials"},
		{auth, "dispatching"},
		{auth, "dispatching"},
		{"auth\n{}\n", "invalid_credentials"},
		{"status\n", "dispatching"},
	} {
		if socketReply(t, path, test.frame) != test.state {
			t.Fatal("unexpected credential handoff state")
		}
	}
	if err := lifeline.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal("fixture did not stop cleanly")
	}
	for _, item := range []string{"started", "stopped"} {
		data, err := os.ReadFile(filepath.Join(dir, "dispatch."+item))
		if err != nil || string(data) != item+"\n" {
			t.Fatal("expected exactly one dispatch start and one completed cancellation")
		}
	}
}

func TestAppSocketRejectsMutationsFromOtherPeer(t *testing.T) {
	dir := socketDirectory(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveAppSocket(ctx, dir, dir, func(context.Context, github.Config) error {
			t.Error("unexpected dispatch for unauthorized peer")
			return nil
		})
	}()
	path := filepath.Join(dir, "helper.sock")
	conn := waitStatusSocket(t, path)
	_ = conn.Close()
	if socketReply(t, path, syntheticAuth(t)) != "invalid_credentials" ||
		socketReply(t, path, "status\n") != "awaiting_credentials" {
		t.Fatal("a client other than the parent changed the session")
	}
	for _, command := range []string{"reload", "quit-prepare", "quit-stop", "quit-resume"} {
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err != nil {
			t.Fatal("socket connection failed")
		}
		defer func() { _ = conn.Close() }()
		if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(conn, command+"\n"); err != nil {
			t.Fatal("socket request failed")
		}
		var reply [1]byte
		if n, err := conn.Read(reply[:]); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("unauthorized %s received a reply or the connection stayed open", command)
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestAuthFrameBoundary(t *testing.T) {
	s := appSession{state: "awaiting_credentials"}
	for _, frame := range []string{strings.Repeat("x", maxAuthLine) + "\n", "{}", "{} {}\n"} {
		if s.authenticate(t.Context(), strings.NewReader(frame)) != "invalid_credentials" {
			t.Fatal("expected incomplete or malformed frame rejection")
		}
	}
}
