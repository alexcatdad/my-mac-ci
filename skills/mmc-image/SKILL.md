---
name: mmc-image
description: Onboard a private repository's Linux arm64 GitHub Actions jobs to the owner's MMC runner on a Mac. Build digest-pinned project images, map job groups to labels and verify one job.
---

Read the project's workflows and toolchain pins. Group jobs by the software and resources they need. Keep jobs that require another architecture or OS on their existing runner. For each MMC group record its label, CPU and memory budget, optional concurrency limit and compatible fallback.

Create the Dockerfile in the project repo. Obtain the current published MMC base digest from the owner or MMC's local image inventory. Pin that digest literally in the Dockerfile; the build supplies the registry prefix.

```dockerfile
ARG MMC_REGISTRY
FROM ${MMC_REGISTRY}/mmc-runner@sha256:<published-base-digest>
USER root
# Install the project's pinned tools here.
USER runner
WORKDIR /home/runner
```

Preserve the inherited runner entrypoint, stdin handoff, runner-version label and non-root runtime user. Keep credentials and registry hostnames outside the build context. Check the project's ignore files before building.

On the runner Mac, from the project checkout, run `mmc image build -name <image-name> <context-directory>` when authorized to publish the image. Record its returned relative `name@sha256:…` reference. The CLI reads the local registry setting and verifies the cached digest.

The owner adds the repo to the GitHub App installation and maps each label to its image, CPUs, memory and optional `max` under `repos["owner/repo"]` in `~/.config/mmc/config.json`. Reload MMC and check the label, digest, runner-version status and isolation in the panel.

Use a repository Actions variable per job group in `runs-on` so the owner can switch that group back independently. A single group can use `${{ vars.CI_RUNNER }}`. Set the variable to the new label before dispatch. Verify one representative job succeeds on that label and the runner disappears after completion. Report the workflow run, digest and any remaining owner action.
