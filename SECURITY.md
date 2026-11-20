# Security Policy

## Reporting a vulnerability

Report vulnerabilities privately through GitHub's
[security advisory form](https://github.com/alexcatdad/my-mac-ci/security/advisories/new).
Do not open a public issue for a suspected vulnerability. Expect an initial
response within seven days.

## Supported versions

MMC is currently distributed as source. Report vulnerabilities against the
current `main` branch and include the affected commit if known.

## What this app does

- Registers ephemeral self-hosted GitHub Actions runners (repo-level runner
  scale sets) for configured repositories, using a GitHub App.
- Holds the GitHub App private key in the Keychain. JIT runner configs and
  tokens reach containers over stdin; they are never logged, never in argv or
  environment.
- Starts one Apple `container` VM per job and removes it after the job.
- Job containers can reach the internet and peers on the shared job network.
  A `pf` rule loaded by a root LaunchDaemon blocks access to the host and
  private, tailnet and link-local destinations outside that network. The job
  kernel disables IPv6. Runners stay stopped until a self-check passes.
- Pulls runner images from the owner's registry and pushes images the owner
  builds.
- No telemetry, analytics or tracking.

## Release trust

The maintainer release workflow builds and signs MMC locally with Developer
ID, notarizes it with Apple, and staples the notarization ticket. Signing keys
and notarization credentials stay in the maintainer's local Keychain.
