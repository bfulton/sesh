# Contributing

Contributions are welcome: issues and pull requests.

- To build, test, and run, see [README.md](README.md).
- Before opening a PR, make sure the tests pass.
- Docs follow [Diátaxis](https://diataxis.fr) and the
  [Google developer documentation style guide](https://developers.google.com/style).
  Each fact lives in one place. Notable changes go in
  [CHANGELOG.md](CHANGELOG.md), in the
  [Keep a Changelog](https://keepachangelog.com) format.
- Report a security problem privately, as [SECURITY.md](SECURITY.md)
  describes, not in an issue.

## Release

1. In one pull request, move the changelog's Unreleased entries under the new
   version, and set that version as `VERSION` in `plugin/scripts/sesh` and in
   `plugin/.claude-plugin/plugin.json`.
2. After it merges, create the release on GitHub's release page with a new
   tag on `main`, such as `v0.4.0`, or push the tag.
3. The Release workflow builds the binaries and adds them to the release in
   about two minutes. Until then, the plugin builds sesh with `go install`. If
   the workflow fails, fix it, then run it from the Actions tab with the tag.
