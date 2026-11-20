# MMC (`my-mac-ci`): spec

GitHub repository `alexcatdad/my-mac-ci`. Display name **MMC**.

## Purpose

Run GitHub Actions jobs for configured repositories on an Apple Silicon Mac, one Apple `container` per job, started only when a job needs one. MMC keeps no warm job VMs. Temporary isolation probes may run while the queue is empty and are removed after the check.

Out of scope: x86_64 builds and project-specific image contents above the shared MMC base.

## Architecture

- `macos/`: Swift menu bar app on MenubarKit (pinned by revision). Tray, 340pt panel, notifications, settings, login item via `SMAppService`.
- `cmd/mmc-helper`: Go backend, supervised by the app over a private unix socket. Exits when the app exits; restarted with backoff on crash.
- Runs in the user session only. Apple `container` cannot run from a LaunchDaemon ([apple/container#1514](https://github.com/apple/container/issues/1514), [#2008](https://github.com/apple/container/issues/2008)). After a reboot or power loss, CI stays down until the owner logs in; this is intended.
- Go (installed 1.27.4). Dependency: `github.com/actions/scaleset`, pinned (public preview).

## Scaling (`actions/scaleset`)

- Scale sets are repo level. One scale set per configured repo and label; the scale set name is the `runs-on` label.
- Auth: a GitHub App with repository Administration read/write and Metadata read. Standalone helper authentication is described under Configuration.
- One listener session per scale set (goroutine), using the `listener` package. Reference: `examples/dockerscaleset`.
- Target the last reported `TotalAssignedJobs`, capped at the label's optional `max`. The global CPU and memory limits apply to every start. Rotate among labels with outstanding demand whose next runner fits. A larger runner may wait while smaller ones fit.
- A runner stops counting toward the target at its `JobCompleted`, not when its container exits: GitHub sends the new job count once, in the same message as `JobCompleted`, and nothing when the container exits (counting until exit stalled the queue in the trial).
- On `JobCompleted` the runner exits by itself; force-remove its container only if still present after a 30 s grace (removing at once killed every container, exit 137).
- The listener calls `Scale` only when a message arrives. Keep each scale set's last `TotalAssignedJobs` and re-run the start decision on every container exit and whenever cap capacity frees, so a start deferred by the cap is reconsidered. On a failed start, show the failure in the tray and schedule a retry with backoff even when no containers are running. Recheck demand, capacity and isolation before each attempt.
- A `Scale` error ends `listener.Run` (trial behaviour). MMC logs it, backs off, and restarts that scale set's session instead of exiting.
- After a helper crash, reuse the scale set by name. A 409 when opening a listener session triggers backoff and a tray warning. Only the owner decides whether to recreate the scale set.
- Identify surviving MMC containers, let their active jobs finish, and count their CPU and memory against the global cap until exit.
- `minRunners` = 0. If a queued job is cancelled after a runner starts, allow 30 seconds for another assignment. For a surplus runner, match its name and scale set, then deregister its numeric ID through the scale-set service. Stop the VM only after successful deregistration. A busy response keeps the job running. An ambiguous response keeps the VM and schedules a retry. [ARC scale-down implementation](https://github.com/actions/actions-runner-controller/blob/master/controllers/actions.github.com/ephemeralrunnerset_controller.go#L1107-L1135)
- JIT configs and tokens are secrets: never logged, never in argv or env (env is visible in `container inspect`).

## Container lifecycle

- `container run --rm -i` with the label's image (by digest), CPUs and memory; JIT config on stdin; one job per container.
- `container` does not refuse over-allocation, so MMC enforces the global cap.
- When quitting with active jobs, offer **Finish jobs and quit** (default), **Stop jobs and quit**, and **Cancel**. The default stops new starts and waits for active jobs to finish. Stop jobs and quit terminates active jobs before closing.

## Image contract (what MMC requires of a project image)

- MMC publishes a shared runner base built from the trial's Ubuntu 24.04 arm64 base. Each project pins the MMC base by digest and adds its tools. To adopt a base update, change the pin and rebuild.
- Run as user `runner` with the Actions runner in `/home/runner`. Receive the one-job JIT config over stdin, keep it in memory and exit after one job. Credential values must stay out of arguments and environment variables on both the Mac and the VM. MMC mounts `/run/mmc` as guest tmpfs. The entrypoint requires guest swap to be off, creates a private runner root there, and decodes the configuration files from stdin. It starts the stock listener from that root. The work directory and project tools stay outside tmpfs.
- Nothing network-related: MMC sets IPv6 off and the network from the host (see Network). `/proc/sys` is read-only in the container even for root, and root lacks `CAP_NET_ADMIN`, so a job cannot change networking.
- Label `mmc.runner.version=<runner version>`.
- No secrets baked in.
- Additional software (tools, browsers, databases, Docker) is the project's choice, written with the MMC skill.

## Onboarding skill (for repos that want to run on the Mac)

- This repo ships an agent skill, `skills/mmc-image/`, that an agent in another repo follows to onboard it.
- Pin the skill to a release revision when installing it.
- The skill covers:
  1. Decide what the project's jobs need, and split jobs by need (one label and image per need).
  2. Write the image's Dockerfile in the project repo, meeting the image contract above using the digest-pinned MMC base.
  3. Build and push with MMC's build command; record the digest.
  4. Add the repo, labels, image digests, CPU, memory and optional `max` to the MMC config (on the Mac, by the owner).
  5. Use a separate Actions runner variable per label in multi-label repos, so each group of jobs can switch independently to a compatible fallback. Single-label repos can keep `runs-on: ${{ vars.CI_RUNNER }}`. The owner adds the repo to the GitHub App installation.
  6. Verify: one job runs on MMC; the image's runner version is current.
- The skill holds no hostnames or secrets; the registry comes from the owner's local config.

## Configuration

The app, helper and CLI share `~/.config/mmc/config.json`. Go uses `encoding/json`. Both global limits must be set before any job starts. The values below are examples.

```json
{
  "registry": "registry.example.com/ci",
  "cap": { "cpus": 8, "memory": "16g" },
  "repos": {
    "owner/repo": {
      "mac-ci-bun": { "image": "ci-bun@sha256:...", "cpus": 2, "memory": "2g" },
      "mac-ci-browser": { "image": "ci-browser@sha256:...", "cpus": 4, "memory": "4g", "max": 1 }
    }
  }
}
```

`mmc-helper run` reads that file by default. Set `MMC_GITHUB_APP_CLIENT_ID` and `MMC_GITHUB_APP_INSTALLATION_ID` in its environment and pipe the PEM key to stdin from the Keychain reader. The helper accepts no key argument. For `run`, add an `isolation` object with the trusted cached probe image reference (`image`, including its digest), the path to the Linux arm64 probe (`probeBinary`), the Mac's LAN IPv4 address (`hostLAN`), and the tailnet resolver IPv4 address (`tailnetResolver`). Build the guest probe with `task build:probe` and the native macOS helper with cgo enabled.

One helper may run per user, including when using an alternate `-config` file. A second helper exits with status 3. Startup accounts for surviving MMC containers before opening listeners. Incomplete ownership or resource metadata on an MMC-owned container blocks startup until resolved. Listeners wait for a fresh isolation pass.

**Reload** applies valid edits while MMC runs.

- Lowering a global limit lets active jobs finish. New runners must fit within both new limits.
- Removing a label immediately stops new launches for it. Its listener closes after active jobs finish. Jobs still queued for the removed label may remain queued.
- Changing a label's digest lets active jobs finish. If the replacement image cannot be pulled, show the error and pause new starts for that label until the requested digest is available.

## Images

- Run `mmc image build` from the project checkout on the Mac. It reads the registry from local MMC config and performs these steps:
  1. For the MMC base image, run `task build:jit-files` to stage its Linux arm64 decoder. Build the image with `container build` (native arm64).
  2. Tag = hash of build context, parent image digest and runner version.
  3. Push to the configured registry and print the digest for the config.
  4. `container builder stop`.
- Build the CLI with `task build:cli`. From a project checkout, run `mmc image build -name ci-bun path/to/image`. The default name is the checkout directory name and the default context is the current directory. For the shared MMC base, run `bin/mmc image build -base` from this repo.
- The staged context includes tracked working-tree files and non-ignored untracked files. Base mode also includes the generated decoder. The builder applies the context's `.dockerignore`. Use `ARG MMC_REGISTRY` before `FROM ${MMC_REGISTRY}/mmc-runner@sha256:<digest>` to reference the configured registry. The output is the verified, locally cached `name@sha256:…` for the label config.
- Set the registry and namespace in local config. For authenticated operations, MMC uses the credentials configured with `container registry login`.
- Jobs use the locally cached configured digest. A registry outage allows starts for labels whose configured image is already available.
- Panel warns when an image's `mmc.runner.version` is behind GitHub's latest runner release (GitHub stops assigning jobs to runners that fall too far behind).

## Secrets

- The owner stores the Mac’s GitHub App private key as a generic password in the login Keychain, with service `com.alexcatdad.my-mac-ci` and account `github-app-mac`. The app reads the two MMC IDs from `github-app.env` under `~/Library/Application Support/my-mac-ci/`. It reads the private key from the Keychain and sends the credentials over its private socket to the helper. Both processes verify their peer before the handoff. The retrieved key stays in memory. For the standalone trial, pipe the Keychain reader’s output directly to helper stdin.
- Nothing secret in config, logs, the repo or images.
- The standalone helper reads the key only from stdin and never sources a credentials file.

## Health and UI

- Tray: OK, or attention badge when any scale set has had no successful GitHub round in 5 minutes, a container start fails, or an image is behind on runner version.
- Notifications via MenubarKit's incident de-duplicator: one on failure, one on recovery.
- Panel row per scale set: state (idle / starting / running n), last GitHub contact, image digest, runner-version warning.

## Network

- Jobs use the shared `mmc` network. Concurrent jobs may connect to each other for now. Jobs must be able to reach the internet and must be blocked from the host, LAN, VLANs, tailnet and containers on other networks.
- MMC itself (host) reaches GitHub, including the Actions broker `broker.actions.githubusercontent.com` (needed by the scale-set session and by every runner), and the configured registry. Image pulls run on the host, so job isolation does not affect them.
- In the trial, a container on the `default` NAT network could reach the internet, the test registry over LAN and tailnet, and the tailnet resolver.
- IPv6: a container gets only a link-local address and no IPv6 route, so no IPv6 path to internet, tailnet or LAN. MMC removes IPv6 entirely with `container run --kernel-arg ipv6.disable=1` (verified: no IPv6 in the container).
- Do not rely on `--internal`: it lets all outbound TCP through, NAT'd ([#2062](https://github.com/apple/container/issues/2062); fix [#2509](https://github.com/apple/container/pull/2509) open, unmerged).
- `pf` works when rules block in the `in` direction with the container subnet as source (reported working in [discussion #719](https://github.com/apple/container/discussions/719); a draft `container system pf` on a fork does this in `/etc/pf.anchors/com.apple.container`, not released). [#1320](https://github.com/apple/container/issues/1320) reports `pf` not filtering vmnet traffic; the trial's `in`-direction rule did filter it.
- Use the shared subnet covered by MMC's fixed `pf` anchor. Launch with `--network mmc --dns 1.1.1.1 --kernel-arg ipv6.disable=1`. The public resolver avoids the host's MagicDNS forwarding.
  - The trial used the `com.apple/050.mmc` anchor under Apple's existing ruleset. Its inbound quick-block rule matched the trial subnet as source and blocked private, shared, link-local and multicast IPv4 destinations (see `trial/net/mmc.pf.conf`). The anchor must cover the shared job subnet. Load it with `pfctl -a com.apple/050.mmc -f <file>` and retain the `pfctl -E` token.
  - Loading at boot: a root LaunchDaemon shipped in the app bundle (`Contents/Library/LaunchDaemons/`) and registered with `SMAppService.daemon(plistName:)`; the owner approves it once in System Settings → Login Items. At load it runs the equivalent of `trial/net/pf.sh load`: `pfctl -a com.apple/050.mmc -f <bundled mmc.pf.conf>` and `pfctl -E`, keeping the token for its lifetime. It does nothing else (no network calls, no other files). Verify `SMAppService` daemon registration on this macOS; fallback: a one-time `sudo` install script placing the plist in `/Library/LaunchDaemons/`.
  - Rule changes ship as app updates. Creating the shared job network needs no root. Refuse to use a subnet already taken by another network.
  - The user-session helper checks behaviour because reading the active rules requires root. Check at startup, after wake or a network change, and hourly, including while idle. Remove the temporary probes afterward. Show the attention badge and block new starts until the check passes. A confirmed isolation failure stops active jobs. An inconclusive check lets active jobs finish.
  - Run the control probe on the default network and the test probe on a job network. Use 5 s TCP connect timeouts. GitHub on port 443 must be reachable from both. A listener container on the default network, the helper listener on the Mac's LAN address, and the tailnet resolver on TCP port 53 must be reachable from the control and blocked from the job network. IPv6 must be absent on the job network.
  - The helper binds its listener on all IPv4 interfaces. For the job gateway check, require a successful job-to-GitHub connection, a successful control-to-listener connection via the Mac’s LAN address, and a blocked job-to-listener connection via the job gateway. A successful job-to-gateway connection is an isolation failure. Repeat the job-side before/after firewall trial when the job network or firewall configuration changes.
  - Count a destination as blocked only after a positive control confirms its listener is reachable. An unreachable control, including a helper listener denied by the macOS firewall, makes that result inconclusive.
  - The check and its last result (time, each target, pass/fail/inconclusive) are shown in the panel.
- Probe results 2026-11-19 (TCP connect; before pf → with pf): internet by name and IP open → open; registry over tailnet, LAN registry, Mac via job gateway, Mac via LAN address, Tailscale resolver open → blocked; IPv6 absent.
- Job to job on the same `mmc` network: **open** with `pf` loaded. Same-subnet traffic is bridged and does not reach the `pf` rule. A container on `default` could not reach it. This peer reachability is accepted for now.
- Every `container` call from MMC has a timeout: a wedged macOS `pfd` can hang all `container` commands forever ([#2275](https://github.com/apple/container/issues/2275), macOS 27).
- A Tailscale exit node on the Mac removes the container subnet's route and it does not recover ([#1881](https://github.com/apple/container/issues/1881)); do not use an exit node on this Mac.

## Trial (before building the app)

1. Done 2026-11-19: `container` 1.7.1 via Homebrew (`brew install container`; no `.pkg` needed), `container system start` with the recommended kernel.
2. Done 2026-11-19: trial image (Ubuntu 24.04 by digest, runner 2.338.2 checksum-verified) built in ~1 min and pushed to the test registry (203 MB). `container run` of a short command: ~0.9 s.
3. Done 2026-11-19: trial scaler (Go, `actions/scaleset` v0.5.0, adapted from `examples/dockerscaleset` to `container run --rm -i`) against this repo, label `mmc-trial`, `.github/workflows/trial.yml` (20 jobs: checkout + 20 s sleep).
4. Done 2026-11-19, 2 CPU / 2 GB per container:
   - Container start to runner connected ~4 s; to job running ~6 s; next container ~1 s after the previous exits.
   - Runner lifetime per job 32–38 s (job ~26 s). Peak guest memory 330–355 MB.
   - 5 at a time: 20 jobs in 2 min 17 s, all exited cleanly; no hangs in ~65 container runs.
   - Scaler behaviour required by these runs is in Scaling (`JobCompleted` counting, 30 s grace).
   - Restart: deleting a scale set and creating one with the same name picks up still-queued jobs.
5. Done 2026-11-19: `container run -i` forwards stdin.
6. Done 2026-11-19: the trial verified external connectivity, the tested host and private-network blocks, and IPv6 removal. Peers on the shared job network remained reachable (see Network).
   - Full run on the `mmc` network with `pf` loaded: 20 of 20 jobs succeeded (checkout, DNS via 1.1.1.1, broker), peak guest memory 331–361 MB.
   - `kill -9` of the scaler mid-run: 4 running containers finished their jobs; the trial scaler recovered by recreating the scale set and ran the remaining 11 jobs.
7. Deferred until a project needs a browser image: check Chromium's sandbox (user namespaces) in Apple's kernel, and whether the `-debug` default kernel costs performance.
8. In a loopback-only VM, stock runner 2.338.2 loaded a synthetic access token from the RAM-backed runner root. Both decoded files matched the input and had mode 0600. Guest swap was off. The token and encoded bundle were absent from the sampled listener arguments, environment and startup log. Missing tmpfs and an unexpected filename both blocked startup.
9. On 2026-11-19 the locally built image started its packaged entrypoint in a loopback-only VM without host mounts. Stock runner 2.338.2 loaded the synthetic credential from guest tmpfs. The packaged decoder and entrypoint hashes matched the build inputs. Omitting the RAM mount blocked startup.
10. A registry tag and digest pull were verified against the tested base image.

### Trial assets (`trial/`)

- `trial/image/`: Dockerfile and runner entrypoint. The original measured trial forwarded the JIT config in a guest command-line argument. The current entrypoint uses stdin and guest tmpfs.
- `trial/scaler/`: Go, `main.go` (~250 lines) with both fixes; the starting point for `cmd/mmc-helper`. Run with `-url`, `-name`, `-image`, `-cpus`, `-memory`, `-max` and the three `MMC_GITHUB_APP_*` variables. It uses the shared trial network. Rebuild the image before using the RAM-backed entrypoint.
- `trial/net/`: `mmc.pf.conf`, `pf.sh` (load/unload/status, sudo), `probe.sh` (reachability probe; addresses passed as arguments).

### Menu-bar app trial

- The menu-bar app passed the isolation gate and completed a one-job live trial with Keychain credentials.

## Open

- Revisit job-to-job isolation later. For now, concurrent jobs may communicate on the shared `mmc` network.
- Local Network Privacy errors from a user-session process reaching container IPs ([#1514](https://github.com/apple/container/issues/1514)); check whether a signed app bundle avoids them.
