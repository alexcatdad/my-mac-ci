// Command mmc-pf maintains the fixed MMC packet-filter anchor at boot.
package main

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	netrules "github.com/alexcatdad/my-mac-ci/trial/net"
)

func pf(input string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/sbin/pfctl", args...)
	cmd.Env = []string{"LC_ALL=C"}
	cmd.Stdin = strings.NewReader(input)
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func run(ctx context.Context) bool {
	if _, err := pf(netrules.Rules, "-a", "com.apple/050.mmc", "-f", "-"); err != nil {
		return false
	}
	out, err := pf("", "-E")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(out, "\n") {
		prefix, token, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(prefix) != "Token" {
			continue
		}
		token = strings.TrimSpace(token)
		if _, err := strconv.ParseUint(token, 10, 64); err != nil {
			return false
		}
		<-ctx.Done()
		// Release this daemon's reference only. Leave the protective rules in place.
		_, err := pf("", "-X", token)
		return err == nil
	}
	return false
}

func main() {
	if os.Geteuid() != 0 || len(os.Args) != 1 {
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !run(ctx) {
		os.Exit(1)
	}
}
