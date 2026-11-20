package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/alexcatdad/my-mac-ci/internal/isolation"
)

func checkIsolation(args []string) int {
	flags := flag.NewFlagSet("mmc-helper check-isolation", flag.ContinueOnError)
	var opts isolation.Options
	flags.StringVar(&opts.ProbeBinary, "probe", "", "path to the Linux arm64 mmc-probe executable")
	flags.StringVar(&opts.Image, "image", "", "locally cached trusted probe image, pinned by digest")
	flags.StringVar(&opts.HostLAN, "host-lan", "", "host LAN IPv4 address for the control listener")
	flags.StringVar(&opts.TailnetResolver, "tailnet-resolver", "", "tailnet resolver IPv4 address to check on TCP port 53")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	report := isolation.Check(ctx, opts)
	for _, check := range report.Checks {
		fmt.Printf("%s: %s\n", check.Name, check.Status)
	}
	fmt.Printf("Isolation: %s\n", report.Status)
	if report.Status == isolation.Pass {
		return 0
	}
	return 1
}
