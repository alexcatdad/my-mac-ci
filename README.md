# MMC (My Mac CI)

MMC is a macOS menu bar app that uses your Apple Silicon Mac to run Linux arm64 GitHub Actions jobs. It starts a fresh Apple container VM when a job is queued and removes it after the job finishes.

[Project page](https://alexcatdad.github.io/my-mac-ci/) · [CI](https://github.com/alexcatdad/my-mac-ci/actions/workflows/ci.yml) · [Spec](docs/SPEC.md) · [MIT license](LICENSE)

## Getting started

MMC is available as source. To run jobs you need an Apple Silicon Mac with macOS 26 or later, Apple's `container` CLI, a GitHub App installed on the repositories you want to serve, and a registry for your runner images. To build from source, you also need Xcode with Swift 6, Go 1.27.4, and Task.

1. Run `task dev:app` to build the app and helper at `macos/.build/dev/MMC Dev.app`. Build the image CLI and Linux arm64 isolation probe with `task build:cli` and `task build:probe`. Run `task check` for local verification with `golangci-lint` installed.
2. Start Apple `container` with `container system start`. Configure `~/.config/mmc/config.json` with global CPU and memory limits, the repositories and labels to serve, and the isolation probe. Set up the GitHub App credentials in Application Support and the login Keychain. See [configuration](docs/SPEC.md#configuration) and [credential storage](docs/SPEC.md#secrets).
3. Follow the [repository onboarding skill](skills/mmc-image/SKILL.md) to build and push a runner image, pin its digest in MMC, and point a workflow job at the configured label.
4. Set up the job network and firewall as described in the [network spec](docs/SPEC.md#network). For the signed app's firewall setup, see [Run a signed bundle](#run-a-signed-bundle). Confirm that the isolation self-check passes in the MMC panel before running jobs.

## Architecture and limits

The Swift menu bar app supervises a Go helper in your login session. The helper listens for jobs through a GitHub Actions scale set per repository and label. It starts runners as demand and the configured CPU and memory limits allow. See [architecture](docs/SPEC.md#architecture) and [scaling](docs/SPEC.md#scaling-actionsscaleset).

- Jobs run on Linux arm64. MMC requires an active macOS login session, including after a reboot.
- The network self-check must pass before new runners can start. Concurrent jobs on MMC's shared network can communicate with each other. See the [network isolation details](docs/SPEC.md#network).

## Run a signed bundle

If you build a signed and notarized bundle, use these steps to run it.

1. Place the notarized `MMC.app` in `/Applications`. Quit any development copy before opening it. If jobs are active, choose **Finish jobs and quit** to let them finish.
2. Open MMC. It reads the repo and label definitions from `~/.config/mmc/config.json`. The release includes the Linux arm64 isolation probe at `/Applications/MMC.app/Contents/Resources/mmc-probe`, suitable for the config's `isolation.probeBinary`.
3. MMC reads the GitHub App IDs from its Application Support folder and the private key from the login Keychain. If macOS needs your approval, click **Try Again** in MMC. The Keychain prompt identifies the signed app.
4. Open MMC Settings and choose **Enable Firewall at Boot**. Approve the background item in System Settings when requested. Enable **Launch at login** if MMC should start serving jobs automatically after you log in.
5. Check the isolation result and each label's last GitHub contact in the panel. After editing the config, reload it in MMC. Notifications are optional and sound is off by default.

The bundled image CLI is at `/Applications/MMC.app/Contents/Helpers/mmc`. Project onboarding and configuration are described in the [image skill](skills/mmc-image/SKILL.md) and the [spec](docs/SPEC.md).

## Review runner sizes

Run `/Applications/MMC.app/Contents/Helpers/mmc sizing review` to see how much CPU and memory each workload used. MMC samples active jobs and keeps summaries locally. The review groups jobs by workload, triggering event, image digest, and allocation, so an aggregate check has a separate baseline from a build on the same label. The affected packages in a pull request can still vary by change.

To identify a particular workload, pass its workflow path and GitHub job display name, for example `sizing review -workflow .github/workflows/ci.yml -job fast`.

Review a workload after 35 successful runs with sufficient sample coverage. Use sustained CPU demand, sampled memory peaks, failures, and elapsed time to choose a change. Apply an approved allocation in the label’s configuration and reload MMC. New jobs use the updated allocation and start a fresh baseline.

## Maintainer releases

The release tasks require a maintainer's local release-tools v0.4.1 installation, a Developer ID Application identity, and an Apple notary profile configured for `my-mac-ci`. Run `task release:check` for preflight and `task release:prepare` to build, sign, notarize and staple a local release. `task release` publishes the prepared release.
