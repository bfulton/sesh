# Changelog

All notable changes to sesh are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Sesh has no releases yet; every change so far is unreleased.

## [Unreleased]

### Added

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

### Changed

- Sessions are identified by session id alone, and share tokens derive from
  it. Vaults created before this change are unsupported: a session can be
  listed twice, and its old share token stops following new saves. Create a
  new vault. (#4)
- A session's catalog entry shows its latest folder and branch. (#4)

### Fixed

- A restored session no longer takes over the original's Remote Control
  bridge, worktree or artifact comment monitors. (#3, #2)
- `sesh save -transcript FILE` saves under the file's session id, not the
  running session's. (#3)
- Compaction summaries no longer appear as messages in search and rendered
  history. (#3)
- Sync no longer alternates between two transcripts that share a session id.
  (#4)
