package main

import (
	"bufio"
	"cmp"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"

	"github.com/alexcatdad/my-mac-ci/internal/config"
	"github.com/alexcatdad/my-mac-ci/internal/usage"
)

const reviewJobs = 35

type cohort struct {
	label, workload, event, image string
	cpus                          int
	memory                        int64
}

func sizingReview(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("sizing review", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	workflow := flags.String("workflow", "", "workflow path such as .github/workflows/check.yml")
	job := flags.String("job", "", "GitHub job display name")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || (*workflow == "") != (*job == "") {
		return errors.New("usage: mmc sizing review [-workflow PATH -job DISPLAY_NAME]")
	}
	dir, err := usage.DefaultDir()
	if err != nil {
		return err
	}
	records, err := usage.Read(dir)
	if err != nil {
		return err
	}
	labels := make(map[string]string)
	matches := make(map[string]bool)
	// Historical observations remain readable after a label or config is removed.
	path, err := config.DefaultPath()
	if err == nil {
		var cfg config.Config
		cfg, err = config.Load(path)
		if err == nil {
			for repo, entries := range cfg.Repos {
				for label := range entries {
					labels[usage.LabelGroup(repo, label)] = repo + " / " + label
					if *workflow != "" {
						matches[usage.WorkloadGroup(repo, label, *workflow, *job)] = true
					}
				}
			}
		}
	}
	if *workflow != "" {
		if err != nil {
			return errors.New("workflow and job lookup requires a readable MMC configuration")
		}
		records = slices.DeleteFunc(records, func(r usage.Record) bool { return !matches[r.Group] })
	}
	return writeSizing(out, records, labels)
}

func sufficientlySampled(r usage.Record) bool {
	duration := r.FinishedAt.Sub(r.StartedAt)
	if r.Samples < 3 || duration <= 0 || r.SampledFrom.IsZero() || !r.SampledThrough.After(r.SampledFrom) {
		return false
	}
	// Exclude coverage outside broker job timestamps, including startup or
	// completion events that arrived late.
	outside := max(0, r.StartedAt.Sub(r.SampledFrom)) + max(0, r.SampledThrough.Sub(r.FinishedAt))
	return float64(r.CoveredMillis-outside.Milliseconds()) >= 0.70*duration.Seconds()*1000
}

func writeSizing(out io.Writer, records []usage.Record, labels map[string]string) error {
	groups := make(map[cohort][]usage.Record)
	unknown := 0
	for _, r := range records {
		if !r.WorkloadKnown || r.Group == "" || r.Event == "" || r.Event == "unknown" || r.ImageDigest == "" || r.CPUs <= 0 || r.MemoryBytes <= 0 {
			unknown++
			continue
		}
		k := cohort{r.LabelGroup, r.Group, r.Event, r.ImageDigest, r.CPUs, r.MemoryBytes}
		groups[k] = append(groups[k], r)
	}
	keys := make([]cohort, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b cohort) int {
		return cmp.Or(cmp.Compare(labels[a.label], labels[b.label]), cmp.Compare(a.label, b.label), cmp.Compare(a.workload, b.workload), cmp.Compare(a.event, b.event), cmp.Compare(a.image, b.image), cmp.Compare(a.cpus, b.cpus), cmp.Compare(a.memory, b.memory))
	})
	w := bufio.NewWriter(out)
	_, _ = fmt.Fprintf(w, "Sizing review uses the latest %d successful jobs per workload, event, image and allocation. Each needs at least 3 samples covering 70%% of its duration.\n", reviewJobs)
	if unknown > 0 {
		_, _ = fmt.Fprintf(w, "Incomplete workload, event, image or allocation identity: %d excluded.\n", unknown)
	}
	if len(keys) == 0 {
		_, _ = fmt.Fprintln(w, "No comparable jobs recorded.")
	}
	for _, k := range keys {
		label := labels[k.label]
		if label == "" {
			label = k.label
		}
		_, _ = fmt.Fprintf(w, "\nLabel %q | workload %s | event %s\nImage %s | allocation %d CPU, %.1f MiB\n", label, k.workload, k.event, k.image, k.cpus, float64(k.memory)/(1<<20))
		if k.event == "pull_request" || k.event == "pull_request_target" {
			_, _ = fmt.Fprintln(w, "The affected work can vary with each pull request.")
		}
		writeCohort(w, groups[k], k)
	}
	return w.Flush()
}

func writeCohort(out *bufio.Writer, records []usage.Record, k cohort) {
	slices.SortStableFunc(records, func(a, b usage.Record) int { return b.FinishedAt.Compare(a.FinishedAt) })
	var average, peakCPU, memory, seconds []float64
	var eligible, failed, other, insufficient int
	for _, r := range records {
		switch {
		case r.Outcome == "failure":
			failed++
		case r.Outcome != "success":
			other++
		case !sufficientlySampled(r):
			insufficient++
		default:
			eligible++
			if eligible > reviewJobs {
				continue
			}
			average = append(average, r.AverageCores*100/float64(k.cpus))
			peakCPU = append(peakCPU, r.PeakCores*100/float64(k.cpus))
			memory = append(memory, float64(r.PeakMemoryBytes))
			seconds = append(seconds, r.FinishedAt.Sub(r.StartedAt).Seconds())
		}
	}
	_, _ = fmt.Fprintf(out, "%d eligible successes; %d failed, %d other outcomes, %d insufficiently sampled successes.\n", eligible, failed, other, insufficient)
	if eligible < reviewJobs {
		_, _ = fmt.Fprintf(out, "Collect %d more eligible successes before review.\n", reviewJobs-eligible)
		return
	}
	for _, v := range [][]float64{average, peakCPU, memory, seconds} {
		slices.Sort(v)
	}
	// Nearest-rank percentiles, with one vote per job regardless of its length.
	p95 := (95*reviewJobs+99)/100 - 1
	last := reviewJobs - 1
	_, _ = fmt.Fprintf(out, "Latest %d eligible jobs: CPU mean p95 %.1f%%, CPU peak p95 %.1f%% (max %.1f%%) of allocation.\n", reviewJobs, average[p95], peakCPU[p95], peakCPU[last])
	_, _ = fmt.Fprintf(out, "Memory peak p95 %.1f MiB (%.1f%%), max %.1f MiB (%.1f%%). Duration median %.1fs, p95 %.1fs.\n", memory[p95]/(1<<20), memory[p95]*100/float64(k.memory), memory[last]/(1<<20), memory[last]*100/float64(k.memory), seconds[last/2], seconds[p95])
	var guidance string
	switch {
	case failed > 0 || other > 0:
		guidance = "Inspect unsuccessful or unknown outcomes for resource pressure before changing allocation."
	case peakCPU[p95] >= 90 || memory[p95] >= 0.90*float64(k.memory):
		guidance = "Usage reached 90% of its allocation at p95. Review the busy resource and job duration before trialing a larger allocation."
	case peakCPU[last] < 50 && memory[last] < 0.50*float64(k.memory):
		guidance = "All observed CPU and memory peaks were below 50%. Consider a smaller trial allocation and compare its durations and outcomes."
	default:
		guidance = "No consistent high usage or uniformly low peaks at these thresholds."
	}
	_, _ = fmt.Fprintln(out, guidance)
}
