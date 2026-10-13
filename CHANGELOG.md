# Changelog

All notable changes to sesh are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [0.4.0] - 2026-10-13

### Added

- Saves replace the secrets they find with placeholders, such as
  `[REDACTED:github-pat]`, before encrypting: API keys, tokens, private keys
  and others that gitleaks' default rules recognize, in the transcript, tool
  outputs, subagent transcripts, memory and the lines the working copy adds.
  `sesh save` says how many it replaced, and `sesh restore` names the files
  restored with placeholders. A session's first save scans all of it; later
  saves scan only what changed. `redact=off` in the config, or
  `SESH_REDACT=off`, saves secrets as they are.
- A save no longer records credentials in its repository's remote URL,
  such as a token in an HTTPS remote or its query.
- The `list_messages` MCP tool lists every message of one kind, by default
  everything the user wrote, with the total count, so an agent can answer
  questions about a whole history, such as how often something happens,
  instead of sampling search results.

### Changed

- Building sesh needs Go 1.24.11 or later, which gitleaks requires.

### Security

- github.com/ulikunitz/xz is v0.5.15 and github.com/klauspost/compress
  v1.18.7, which fix GO-2025-3922 and GO-2026-5841. Both came with gitleaks;
  govulncheck finds no vulnerable code that sesh calls.

### Fixed

- The Release workflow adds the binaries to a release created on GitHub's
  release page, instead of failing because the release exists. It can also be
  run by hand for a tag.

## [0.3.0] - 2026-10-12

The first release.

### Added

- Release binaries for Linux, macOS and Windows on amd64 and arm64, built
  for each `v*` tag with a `SHA256SUMS` file. The plugin downloads the one
  for its system and checks it, instead of compiling sesh with Go, which took
  minutes and timed out a cloud session's MCP server; it falls back to
  `go install`.
- `sesh version`.
- Keep every Claude Code session automatically, through the plugin's `Stop`
  hook, or with `sesh sync` and `sesh watch`. Sessions are encrypted on the
  device into a folder, an S3-compatible bucket, or a sesh server. (#1)
- Continue a session with `sesh restore`: its working copy on a new branch and
  its conversation as a session that `claude --resume` continues. (#1)
- Search sessions with `sesh search`, `sesh list` and `sesh show`, and from any
  agent through the MCP server. (#1)
- `sesh serve`, storage that only sees ciphertext. (#1)
- Saves carry the session's large tool outputs and subagent transcripts, and
  restores point the conversation at the restored copies. (#3)
- `sesh restore` says how much of a compacted session `claude --resume` loads.
  (#3)
- CI: formatting, `go vet` and race-detector tests on Linux and macOS, builds
  for Linux, macOS and Windows, and `shellcheck` on the plugin launcher. (#4)
- Contributing guide, security policy, changelog, and a reference page. (#5)
- A restored session joins this device's library, so sesh's search tools can
  read what its rendered history left out, even for a session restored from
  someone else's share token. The MCP server sees it at once.
- `sesh status` on the command line: the vault, the storage and the last hook
  run.
- `/sesh status`, `list`, `search`, `save` and `restore` in any Claude Code
  session with the plugin. Claude shows what sesh printed, errors included;
  after a restore in a cloud session, it says where the session stopped, and
  the whole history joins the session when that turn ends. Arguments reach
  sesh as data, never through a shell.

### Changed

- Sessions are identified by session id alone, and share tokens derive from
  it. Vaults created before this change are unsupported: a session can be
  listed twice, and its old share token stops following new saves. Create a
  new vault. (#4)
- A session's catalog entry shows its latest folder and branch. (#4)
- A rendered history keeps the conversation over tool output: tool calls and
  results shrink to one-line stubs before any message is left out. Its
  default size is 100 KB, about 45,000 tokens, down from 300 KB.
- Inside a Claude Code session, `sesh key` refuses and `sesh init` doesn't
  print the vault key: the session's transcript would keep it. In a cloud
  session, `sesh init` refuses, since the vault would be lost with the
  session; set `SESH_KEY` and `SESH_STORE` in the environment instead.
- In a cloud session, `sesh restore` prints the history for the model to
  read, since a session for `claude --resume` would never be read there.

### Removed

- `sesh save`, `sesh restore`, `sesh search` and `sesh status` typed as
  prompts, and the `UserPromptSubmit` hook that caught them. Their replies
  never showed in the Claude app; use `/sesh` instead.

### Fixed

- A restored session no longer takes over the original's Remote Control
  bridge, worktree or artifact comment monitors. (#3, #2)
- `sesh save -transcript FILE` saves under the file's session id, not the
  running session's. (#3)
- Compaction summaries no longer appear as messages in search and rendered
  history. (#3)
- Sync no longer alternates between two transcripts that share a session id.
  (#4)
- The plugin no longer runs a `sesh` it finds on `PATH` or in `~/go/bin`,
  which could be another tool of the same name or a different version. It
  runs `SESH_BIN`, or the copy it installed.

[Unreleased]: https://github.com/bfulton/sesh/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/bfulton/sesh/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/bfulton/sesh/releases/tag/v0.3.0
