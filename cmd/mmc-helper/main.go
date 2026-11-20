// Command mmc-helper supervises job runners and checks local configuration and isolation.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/alexcatdad/my-mac-ci/internal/config"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: mmc-helper {--app-mode|run|check-config|check-isolation} [options]")
		return 2
	}
	switch args[0] {
	case "--app-mode":
		return appMode(args[1:])
	case "run":
		return runHelper(args[1:])
	case "check-config":
		return checkConfig(args[1:])
	case "check-isolation":
		return checkIsolation(args[1:])
	default:
		fmt.Fprintln(os.Stderr, "unknown command (use --app-mode, run, check-config or check-isolation)")
		return 2
	}
}

func checkConfig(args []string) int {
	flags := flag.NewFlagSet("mmc-helper check-config", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	path := flags.String("config", "", "path to the MMC JSON configuration")
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
	if *path == "" {
		var err error
		*path, err = config.DefaultPath()
		if err != nil {
			fmt.Fprintln(os.Stderr, "cannot resolve the configuration path")
			return 1
		}
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	labels := 0
	for _, repo := range cfg.Repos {
		labels += len(repo)
	}
	fmt.Printf("Configuration valid (%d repos, %d labels).\n", len(cfg.Repos), labels)
	return 0
}
