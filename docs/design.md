# Sesh design

Sesh keeps a private copy of every agent session you have, so you can continue any of them anywhere and search across all of them. It does three things:

- **Keep.** Each session's conversation and uncommitted work are saved, encrypted on your device, into storage you choose.
- **Continue.** A saved session comes back anywhere: its working copy on a new branch, its conversation either loaded into the current session or written as a session the CLI resumes with real turns.
- **Remember.** A local index answers searches across every session, for you and, over MCP, for agents.

Storage only ever holds ciphertext. Plaintext exists only on your devices and in the model context you choose to load it into.

This page explains why sesh works as it does. Commands, settings and formats are in the [reference](reference.md).

## Verified behavior

Checked on October 10 and 11, 2026, in Claude Code cloud sessions and with the Claude Code CLI 2.1.296.

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
| It works on a real history | On the author's machine: 14 sessions synced (incremental re-sync 0.03s); "RoomPlan" ranked the session holding that work first; a fresh `claude -p` with only the MCP server answered a design question from messages 3389 and 3424 of a 1,960-message session, in 6 tool calls and about 33s |
| The server never sees plaintext | After syncing 14 real sessions through `sesh serve`, its files contained none of their text |

## Keys and tokens

Everything derives from one **vault key** per person ([derivations](reference.md#keys-and-tokens)). It is written in lowercase Crockford base32: letters and digits only, so a phone double-tap selects the whole thing, and none of the characters that are easy to misread.

Because share tokens are derived, every device with the vault key saves a session under the same token with no local state. That's what makes keeping cloud sessions work: their containers keep nothing between runs. Without a vault, `sesh save` falls back to a random token kept on the device.

## Storage

Storage holds two kinds of object ([layout](reference.md#storage)): one archive per session, sealed under the session's share token, and one catalog entry per session, sealed under a key derived from the vault key. A catalog entry's object name is bound into its encryption, so an entry copied to another name doesn't decrypt. The catalog is written after the archive, so an entry never points at a missing archive.

A session is identified by its session id alone; its folder is metadata on the entry. Claude Code moves a session's transcript to another project folder when the session changes folder (`relocateSessionTranscript`, which stamps a `relocated` record), so the folder isn't stable over a session's life. A session's catalog entry is named from its id, so each save replaces the last. The two transcripts that can share an id at once (one mid-move, or a nested `claude` that inherited its parent's id through the environment) resolve to the most recently modified.

Claude Code keeps a folder beside each transcript, `<session id>/`. When a tool's output is too large for the context, it is written to `tool-results/` and the transcript says where, by absolute path; subagent transcripts are in `subagents/`. Saves carry those two parts, smallest files first. The rest of the folder, such as a cloud session's `ccr-tip.json` event pointer, belongs to the live original and stays behind.

## Keep

- **Stop hook.** After each turn, the plugin's `Stop` hook saves the live session with its working copy. It hands the save to a detached process and returns at once; saves are serialized with a lock so the newest is written last. Detaching replaced `async: true`, which `claude -p` cuts short.
- **`sesh sync` and `sesh watch`.** Save every local transcript whose size or modification time changed since the last save. These save the conversation only: a past session's working copy is long gone. If two project folders hold the same session id, the most recently modified stands for the session.
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

The **native file** is the transcript with each record's `sessionId` and `cwd` rewritten; content, uuids and the `parentUuid` chain are unchanged. It goes in `~/.claude/projects/<cwd>/`, where `<cwd>` has every character other than an ASCII letter or digit replaced by `-`. The memory folder is copied alongside, never overwriting existing files. The session folder's files go in the copy's own folder, subagent transcripts rewritten the same way, and references to the original folder are pointed at the copy's, so a resumed agent can reread a large tool output.

A copy is a fork, so it leaves out what binds the original to live state. Claude Code applies these metadata records to whatever session id they carry, and its own fork clears the same state: `bridge-session` (the Remote Control bridge; a copy that kept it took over the original's bridge, which then dropped off with close 4090, issue #2), `worktree-state`, `relocated`, `dev-mods`, `artifact-comment-monitor` and `artifact-autoreact-ledger`, plus `bridge_status` system messages, which link to the original's bridge. A message left out is unlinked from the chain: its children take its parent. Tested with Claude Code 2.1.296: before the fix, a resumed copy wrote the original's bridge id back out under its own id and tried to enter the original's worktree; after it, neither appears.

`claude --resume` loads a session by following `parentUuid` back from its latest message. A compaction writes a `compact_boundary` record with no parent, so a compacted session resumes from its last compaction: the summary, the prompt of the turn the compaction interrupted (the summary names it in `summarizedTurnPromptUuids`), and what came after. Earlier turns stay in the file, unread; this is how Claude Code treats any compacted session, not something the copy changes. Tested here: a copy of this session, compacted mid-turn at 785k tokens, resumed in a terminal showing 1 of its 60 prompts. Relinking the boundary would load the full history and force another compaction, so restore leaves it alone and says how many prompts the resume brings back; `sesh show` prints the whole conversation. Compaction summaries are left out of the rendered history and the search index, since the turns they summarize are there in full.

The **rendered history** is a quoted record of past turns, framed as history rather than instructions. User and assistant text is kept whole; tool calls and results are trimmed, keeping their start and end; thinking and harness records are dropped. It fits a budget (300 KB by default): tool output is trimmed harder first, then the oldest turns are dropped, with a marker. This session's 3 MB transcript renders to about 300 KB with nothing dropped.

## Remember

The **library** is the device's decrypted copy of the vault: one file per session with its catalog entry and conversation (tool text capped at 4 KB). Opening it pulls sessions that are new or saved since they were cached, eight at a time.

The **index** is in-memory BM25 (k1 = 1.2, b = 0.75) over every message, plus each title, built on open. Terms are lowercase letters and digits; identifiers split at case changes and underscores; simple plurals fold. All query terms must match unless no message has them all, in which case messages with the most terms rank. User messages weigh 1.3, assistant 1.0, tool calls 0.6 and tool results 0.5, so conversation outranks tool output. Searches over this machine's sessions take about 40 ms including the pull.

**MCP** (`sesh mcp`, stdio, JSON-RPC 2.0) offers message-level and session-level [tools](reference.md#mcp-tools). Its instructions tell agents to search messages and then read around them, which the verified behavior above shows works best. If the store can't be reached, it answers from the device's copy and says so.

## Server

`sesh serve` is storage for people without a bucket. It never sees plaintext: archives are readable by name, which is unguessable, and their content is sealed under tokens the server never sees; a vault's catalog is visible only to that vault's owner ([access rules](reference.md#server-access)). Someone who learned a vault id before its owner's first write could claim it and block the owner; vault ids are derived from the key and never published, so this needs a leak.

## Plugin

`plugin/` is a Claude Code plugin, and the repository is also its marketplace ([files](reference.md#plugin-files)). Hooks run on every prompt and every turn, so the launcher must be fast and must never break a session: prompts that don't start with "sesh" return in about 20 ms without touching the binary, and if sesh can't be installed, hooks do nothing and a typed sesh command explains why.

## Out of scope

- Writing into a hosted session's transcript, or its internal ingress endpoint.
- Other agents. Codex's hooks and rollout format are unverified; session-migrate's format research is the reference if sesh adds them.

## Related work

None of these restore into a hosted session, carry the working copy, keep sessions continuously, or move them through remote storage.

- **[session-migrate](https://github.com/xhluca/session-migrate)** converts sessions among 18 local agents into each target's native, resumable format, keeping tool calls and results where both formats support them. It deliberately drops signed thinking, since a provider's signature can't be recreated. Sesh takes no dependency on it.
- **[transplant](https://github.com/compoundingtech/transplant)** moves a Claude Code or Codex session to another directory or machine: a tar of `manifest.json` and the opaque transcript, plus the project memory folder and `~/.claude.json` project entry. Sesh's native restore follows the same design.
- **[ctxmv](https://github.com/Ryu0118/ctxmv)** (v0.7.0) moves sessions between local agents by writing the target's native session file. It works on one machine only, carries no working copy, and keeps only user and assistant text. On one real session that kept 96,000 characters and dropped 147 tool calls and 146 results (about 338,000 characters).
- **[Unified Agent Session Specification](https://github.github.com/gh-aw/specs/unified-agent-session-specification/)** (GitHub Agentic Workflows, draft v1.7.0) defines neutral session events for logging and analysis. It has no working copy and no resume semantics.

## Known limitations

- A restored session paired with Remote Control shows only its last 200 messages (after the latest compaction) on phone and web: Claude Code hard-codes that back-fill cap, so the full history stays available through `sesh show` and the transcript, not Remote Control.

## Open questions

- Whether an account-level plugin install reaches cloud sessions. The docs cover only terminal sessions and Cowork; this session shows a per-account `~/.claude/plugins/synced/` folder, empty because nothing is enabled.
- Which hook output the Claude app displays for a blocked prompt: `reason`, `systemMessage`, or neither. The share token is shown through both.
- Whether a prompt can be submitted at all once an account's quota is exhausted.
- Whether signed thinking blocks in a native session resume under a different account. If not, restore should drop them, as session-migrate does.
- Redacting secrets from tool output before saving. Today a session's archive holds everything its transcript does.
- How large a rendered history can be before the target model's context suffers.
