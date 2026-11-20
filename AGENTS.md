NEVER start a runner until the isolation self-check in docs/SPEC.md passes
NEVER expose registry hostnames, LAN or tailnet addresses, GitHub App keys, JIT configs or tokens in the repo, logs, commits or issues
NEVER pass JIT configs or tokens in arguments or environment variables
NEVER use a plaintext fallback for Keychain credentials
NEVER add shipped third-party dependencies without a recorded decision
NEVER replace published release assets or move release tags
ALWAYS use one Apple container VM per job and remove it after the job
ALWAYS run the runner and helper in the user session
ALWAYS limit the root LaunchDaemon to loading the pf anchor
ALWAYS set a timeout on every container command
ALWAYS use MenubarKit for notifications and request permission from the user toggle, with sound off by default
ALWAYS use MenubarKit's 18pt template tray icon, status badge and status-aware accessibility label
ALWAYS pin MenubarKit and GitHub Actions to full commit SHAs
ALWAYS start workflows from `permissions: {}`
ALWAYS read docs/SPEC.md before building
ALWAYS run `task check` before opening a PR
ALWAYS branch from main, PR to main and use Conventional Commits
ALWAYS build releases locally with release-tools, Developer ID signing, notarization and stapling
ALWAYS use CalVer `vYYYY.MM.DD.N` for releases
IF an action needs sudo or an admin password, changes a GitHub App installation, deletes registry tags or changes a Keychain entry: owner only
IF a CI status is needed for a decision: check once without polling
