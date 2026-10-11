# Sesh design

Sesh keeps a private copy of every agent session you have, so you can continue any of them anywhere and search across all of them. It does three things:

- **Keep.** Each session's conversation and uncommitted work are saved, encrypted on your device, into storage you choose.
- **Continue.** A saved session comes back anywhere: its working copy on a new branch, its conversation either loaded into the current session or written as a session the CLI resumes with real turns.
- **Remember.** A local index answers searches across every session, for you and, over MCP, for agents.

Storage only ever holds ciphertext. Plaintext exists only on your devices and in the model context you choose to load it into.

## What we verified

Checked on 2026-10-10/11 in Claude Code cloud sessions and with the real CLI (2.1.296).

| Finding | Evidence |
| --- | --- |
| A session can read its own transcript | `~/.claude/projects/<cwd>/<session-id>.jsonl`, plain file reads |
| Repo-level hooks fire in cloud sessions | A repo `Stop` hook ran in a cloud session; a repo `UserPromptSubmit` hook ran `sesh restore` in another |
| Writing a hosted session's transcript file changes nothing | Appended records never reached the server or the running model |
| A stopped turn costs nothing | `continue: false` from `UserPromptSubmit`: `num_turns: 0`, session cost unchanged |
| A stopped turn's `stopReason` stays in the model's context | A hosted session recalled a codeword carried only in `stopReason` |
| `stopReason` has no practical size cap | 200,000 characters stored whole; facts at start, middle and end recalled |
| `decision: "block"` does not reach the model's context | A blocked prompt's text was not recalled later |
| The app does not display `stopReason` | The restore turn showed nothing in the Claude app |
| The local CLI resumes a transcript it didn't write | Hand-written session files resume; a restored session recalled a fact seen only in a tool result; signed thinking blocks resumed cleanly (same account) |
| Async hooks don't finish under `claude -p` | The same `Stop` hook saved when synchronous and never ran to completion with `async: true` |
| Plugin hooks and MCP work with `--plugin-dir` | A turn was kept automatically; typed commands ran with no model call; MCP tools `mcp__plugin_sesh_sesh__*` connected |
| Agents use the vault well with message-level tools | Asked about a past decision, a fresh session took 18 turns with session-level tools and 7 ($0.10) with `search_messages` + `read_messages`, citing the exact message |
| The server never sees plaintext | After syncing 14 real sessions through `sesh serve`, its files contained none of their text |

## Keys and tokens

Everything derives from one **vault key** per person: 128 random bits, written as 26 characters of lowercase Crockford base32 (`0-9a-z` without `i l o u`). Letters and digits only, so a phone double-tap selects the whole thing; decoding ignores case and maps `i l`→`1`, `o`→`0`.

HKDF-SHA256 over the vault key derives:

| Output | Info string | Use |
| --- | --- | --- |
| Share token for a session | `sesh session v1:<session id>` | Opens that session's archive; given to others to share one session |
| Vault id (16 bytes, hex) | `sesh vault id v1` | Names the vault's catalog without revealing the key |
| Catalog key (32 bytes) | `sesh catalog key v1` | AES-256-GCM for catalog entries |
| Entry id (16 bytes, hex) | `sesh entry v1:<session id>` | Names a session's catalog entry |
| Server token (32 bytes, hex) | `sesh server access v1` | Bearer token for `sesh serve` |

A share token is itself a token; HKDF over it derives the archive's encryption key (`sesh key v1`) and object id (`sesh id v1`).

Because share tokens are derived, every device with the vault key saves a session under the same token with no local state. That's what makes keeping cloud sessions work: their containers keep nothing between runs. Without a vault, `sesh save` falls back to a random token kept in `~/.sesh/tokens/`.

## Storage layout

Objects have structured names; stores keep them under `sesh/`:

```
v1/s/<archive id>               one session's archive, sealed under its share token
v1/c/<vault id>/<entry id>      one catalog entry, sealed under the catalog key
```

A **catalog entry** is JSON: session id, source, title, working directory, repo, branch, start, last activity, save time, message count, transcript size, share token and host. Its AES-GCM associated data is its object name, so an entry copied to another name doesn't decrypt. The catalog is written after the archive, so an entry never points at a missing archive.

An **archive** is a 12-byte random nonce followed by the AES-256-GCM ciphertext of a gzipped tar:

```
manifest.json                   format version, source, session id, repo, branch, base commit,
                                save time, changed and deleted paths, memory files
transcript.jsonl                the native transcript, verbatim
worktree/patches/<path>.patch   one binary diff per changed path, against base
memory/<path>                   the session's Claude Code project memory folder, if any
```

Stores: a directory (`file:///path`), an S3-compatible bucket (`s3://bucket/prefix`, configured through the AWS SDK's environment), or a sesh server (`https://host`).

## Keep

- **Stop hook.** After each turn, the plugin's `Stop` hook saves the live session with its working copy. It hands the save to a detached process and returns at once; saves are serialized with a lock so the newest is written last. Detaching replaced `async: true`, which `claude -p` cuts short.
- **`sesh sync` / `sesh watch`.** Saves every local transcript whose size or modification time changed since the last save (recorded in `~/.sesh/state.json`). These save the conversation only: a past session's working copy is long gone. If two project folders hold the same session id, the most recently modified keeps it and the others get a suffix from their folder, so no save overwrites another session.
- **`sesh save`.** Saves on demand and shows the share token.

### Working copy

Capture:
1. Base is the newest commit a remote has (`git merge-base HEAD @{upstream}`, falling back to the remote default branch), so the target can fetch it. Unpushed commits travel as file content.
2. Copy the index to a private index file, keeping its modification time, then `git add -A` into it. The user's staging area is untouched. The preserved time matters: git compares stat data at one-second resolution and re-reads an entry only while it's "racy" (not older than the index file). A fresh copy loses that, and an edit made in the same second as staging, at the same size, is staged stale. A test reproduces this and fails if the time isn't preserved; it checks the patch's post-image blob id, because `git diff --cached` reads the working-tree file for the patch body when stat data matches, hiding the stale blob.
3. Each changed path becomes one `git diff --cached --binary --full-index` patch against base. Deletions are listed, not patched.

Restore: fetch base if needed, check it out on a new branch, apply the patches, remove deleted paths. A dirty working copy is refused.

## Continue

Real turns reach only a session started from a session file: a running CLI never re-reads its transcript, and a hosted session's transcript lives on the provider's server. So restore picks the form from where it runs:

| Where `sesh restore` runs | Result |
| --- | --- |
| A shell | A native session file for the target directory; prints `claude --resume <id>` |
| The hook, in a local CLI session | The rendered history as `stopReason`, so this session continues now, plus a native session file for real turns |
| The hook, in a hosted session (`CLAUDE_CODE_REMOTE=true`) | The rendered history as `stopReason` only |

`restore` takes a share token, or with a vault, a session id prefix or title words.

The **native file** is the transcript with each record's `sessionId` and `cwd` rewritten; content, uuids and the `parentUuid` chain are unchanged. It goes in `~/.claude/projects/<cwd>/`, where `<cwd>` has every character other than an ASCII letter or digit replaced by `-`. The memory folder is copied alongside, never overwriting existing files.

The **rendered history** is a quoted record of past turns, framed as history rather than instructions. User and assistant text is kept whole; tool calls and results are trimmed, keeping their start and end; thinking and harness records are dropped. It fits a budget (300 KB by default): tool output is trimmed harder first, then the oldest turns are dropped, with a marker. This session's 3 MB transcript renders to about 300 KB with nothing dropped.

## Remember

The **library** is the device's decrypted copy of the vault in `~/.sesh/cache/<vault id>/`: one file per session with its catalog entry and conversation (tool text capped at 4 KB). Opening it pulls sessions that are new or saved since they were cached, eight at a time; `-offline` skips the store.

The **index** is in-memory BM25 (k1 = 1.2, b = 0.75) over every message, plus each title, built on open. Terms are lowercase letters and digits; identifiers split at case changes and underscores; simple plurals fold. All query terms must match unless no message has them all, in which case messages with the most terms rank. User messages weigh 1.3, assistant 1.0, tool calls 0.6 and tool results 0.5, so conversation outranks tool output. Searches over this machine's sessions take about 40 ms including the pull.

**MCP** (`sesh mcp`, stdio, JSON-RPC 2.0) exposes `search_messages`, `read_messages`, `search_sessions`, `list_sessions` and `get_session`. The server's instructions tell agents to search messages and then read around them. If the store can't be reached, it answers from the device's copy and says so.

## Server

`sesh serve` stores objects in a directory and enforces:

| Name | Read | Write | List |
| --- | --- | --- | --- |
| `v1/s/<id>` | anyone (the name is unguessable; the content is sealed under a token the server never sees) | the bearer that first wrote it | no |
| `v1/c/<vault>/…` | the vault's owner only; others get 404 | the vault's owner only | the vault's owner only; others get an empty list |

A vault's owner is the first bearer to write in it. Bearers are recorded as SHA-256 hashes. Objects are capped at 256 MB. Someone who learned a vault id before its owner's first write could claim it and block the owner; vault ids are derived from the key and never published, so this needs a leak.

## Plugin

`plugin/` is a Claude Code plugin; the repository is also its marketplace.

- `hooks/hooks.json`: `UserPromptSubmit` and `Stop` both run `scripts/sesh hook`.
- `.mcp.json`: the MCP server.
- `scripts/sesh`: finds sesh (`$SESH_BIN`, a copy it installed, or `PATH`) or installs it with `go install …@main` (once per version string; set `SESH_VERSION` to pin a tag or commit). Prompts that don't start with "sesh" return in about 20 ms without touching the binary; if installation fails, hooks do nothing, and a typed sesh command gets the reason.

Typed commands: `sesh save`, `sesh restore <token|id|title>`, `sesh search <words>`, `sesh status`. Every hook run is recorded in `~/.sesh/hook-status.json`.

## Configuration

`~/.sesh/` (or `$SESH_HOME`) holds `config` (`store=<url>`), `key`, `state.json`, `hook-status.json`, `cache/` and `tokens/`. `SESH_STORE` and `SESH_KEY` override the files; cloud sessions use them as environment variables. `SESH_BUDGET` sets the rendered-history size.

## Out of scope

- Writing into a hosted session's transcript, or its internal ingress endpoint.
- Other agents. Codex's hooks and rollout format are unverified; session-migrate's format research is the reference if sesh adds them.

## Related work

None of these restore into a hosted session, carry the working copy, keep sessions continuously, or move them through remote storage.

- **[session-migrate](https://github.com/xhluca/session-migrate)** converts sessions among 18 local agents into each target's native, resumable format, keeping tool calls and results where both formats support them. It deliberately drops signed thinking, since a provider's signature can't be recreated. Sesh takes no dependency on it.
- **[transplant](https://github.com/compoundingtech/transplant)** moves a Claude Code or Codex session to another directory or machine: a tar of `manifest.json` and the opaque transcript, plus the project memory folder and `~/.claude.json` project entry. Sesh's native restore follows the same design.
- **[ctxmv](https://github.com/Ryu0118/ctxmv)** (v0.7.0) moves sessions between local agents by writing the target's native session file. It works on one machine only, carries no working copy, and keeps only user and assistant text. On one real session that kept 96,000 characters and dropped 147 tool calls and 146 results (about 338,000 characters).
- **[Unified Agent Session Specification](https://github.github.com/gh-aw/specs/unified-agent-session-specification/)** (GitHub Agentic Workflows, draft v1.7.0) defines neutral session events for logging and analysis. It has no working copy and no resume semantics.

## Open questions

- Whether an account-level plugin install reaches cloud sessions. The docs cover only terminal sessions and Cowork; this session shows a per-account `~/.claude/plugins/synced/` folder, empty because nothing is enabled.
- Which hook output the Claude app displays for a blocked prompt: `reason`, `systemMessage`, or neither. The share token is shown through both.
- Whether a prompt can be submitted at all once an account's quota is exhausted.
- Whether signed thinking blocks in a native session resume under a different account. If not, restore should drop them, as session-migrate does.
- Redacting secrets from tool output before saving. Today a session's archive holds everything its transcript does.
- How large a rendered history can be before the target model's context suffers.
