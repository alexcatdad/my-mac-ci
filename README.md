# MMC

MMC runs Linux arm64 GitHub Actions jobs on an Apple Silicon Mac. The menu-bar app listens for queued jobs in the user session and starts one temporary Apple container VM per job. Its network check must pass before it starts runners.

## Build from source

Use Apple Silicon, macOS 26 or later, Xcode with Swift 6, Go 1.27.4, and Task. Run `task dev:app` to build the development app at `macos/.build/dev/MMC Dev.app`. Run `task check` for local verification with `golangci-lint` installed. The development bundle contains the app and helper. Build the image CLI and isolation probe separately with `task build:cli` and `task build:probe`.

## Run a signed bundle

MMC is currently distributed as source. If you have a signed and notarized bundle, these steps describe how to run it.

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
