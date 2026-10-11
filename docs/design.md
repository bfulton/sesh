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
| The app does not display a blocked prompt's `reason` or `systemMessage` | A blocked `sesh status` showed nothing in the Claude app, though its full text was recorded |
| What a hook prints is kept | A cloud session's event stream stores each hook's stdout (`hook_response`); a block message is also written to the transcript, which sesh archives |
| An account-level plugin reaches cloud sessions | A cloud session loaded `sesh@synced` from `~/.claude/plugins/synced/`; its `Stop` hook ran every turn, and its MCP server connected once installed |
| A skill's `!` commands put its arguments in a shell | `/sesh restore abc; touch FILE` created `FILE` in auto mode; in the default mode the permission check aborted the skill |
| A `UserPromptExpansion` hook gets a typed command's arguments as data | `command_args` held `;`, `$(…)` and quotes verbatim, nothing ran, and the hook's `additionalContext` reached the model. `command_name` is `plugin:skill` even when typed bare |
| Hook context past about 10,000 characters reaches the model only as a preview | 9,516 characters arrived whole; from 10,516 the model got a 2 KB preview and a file path |
| A failing `!` command aborts the skill; `!` commands time out after 120 seconds | The model never ran, only `Shell command failed for pattern …` was reported; `sesh watch` was moved to the background after 120 s |
| A model given a rendered tail finds the rest with sesh's tools | Asked about two decisions left out of a 30 KB render of a 2 MB session, Haiku answered both with `search_messages` and `read_messages` (7 turns) and couldn't without them; from a 150 KB render it answered in 1 turn |
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
| `/sesh restore` in a local CLI session | The same: a native session file, and the `claude --resume` command |
| `/sesh restore` in a hosted session (`CLAUDE_CODE_REMOTE=true`) | 8 KB of rendered history, which the model reads in that turn |
| `sesh restore` in a hosted session | The rendered history, printed |

Either way the restored session joins this device's library, even when it came from someone else's share token, so sesh's search tools can read what the rendered history trimmed or left out. That lets the history stay small: 100 KB by default, about 45,000 tokens. It keeps every user and assistant message it can, since that is where decisions are, and shrinks tool calls and results to one-line stubs before leaving any message out.

The **native file** is the transcript with each record's `sessionId` and `cwd` rewritten; content, uuids and the `parentUuid` chain are unchanged. It goes in `~/.claude/projects/<cwd>/`, where `<cwd>` has every character other than an ASCII letter or digit replaced by `-`. The memory folder is copied alongside, never overwriting existing files. The session folder's files go in the copy's own folder, subagent transcripts rewritten the same way, and references to the original folder are pointed at the copy's, so a resumed agent can reread a large tool output.

A copy is a fork, so it leaves out what binds the original to live state. Claude Code applies these metadata records to whatever session id they carry, and its own fork clears the same state: `bridge-session` (the Remote Control bridge; a copy that kept it took over the original's bridge, which then dropped off with close 4090, issue #2), `worktree-state`, `relocated`, `dev-mods`, `artifact-comment-monitor` and `artifact-autoreact-ledger`, plus `bridge_status` system messages, which link to the original's bridge. A message left out is unlinked from the chain: its children take its parent. Tested with Claude Code 2.1.296: before the fix, a resumed copy wrote the original's bridge id back out under its own id and tried to enter the original's worktree; after it, neither appears.

`claude --resume` loads a session by following `parentUuid` back from its latest message. A compaction writes a `compact_boundary` record with no parent, so a compacted session resumes from its last compaction: the summary, the prompt of the turn the compaction interrupted (the summary names it in `summarizedTurnPromptUuids`), and what came after. Earlier turns stay in the file, unread; this is how Claude Code treats any compacted session, not something the copy changes. Tested here: a copy of this session, compacted mid-turn at 785k tokens, resumed in a terminal showing 1 of its 60 prompts. Relinking the boundary would load the full history and force another compaction, so restore leaves it alone and says how many prompts the resume brings back; `sesh show` prints the whole conversation. Compaction summaries are left out of the rendered history and the search index, since the turns they summarize are there in full.

The **rendered history** is a quoted record of past turns, framed as history rather than instructions. User and assistant text is kept whole; tool calls and results are trimmed, keeping their start and end; thinking and harness records are dropped. It fits a budget (100 KB by default, about 45,000 tokens): tool output is trimmed harder first, down to a one-line stub per call and result, and only then are the oldest turns dropped, with a marker. A 2 MB design session renders to 100 KB keeping 31 of its 34 prompts; the search tools reach the rest.

## Remember

The **library** is the device's decrypted copy of the vault: one file per session with its catalog entry and conversation (tool text capped at 4 KB). Opening it pulls sessions that are new or saved since they were cached, eight at a time.

The **index** is in-memory BM25 (k1 = 1.2, b = 0.75) over every message, plus each title, built on open. Terms are lowercase letters and digits; identifiers split at case changes and underscores; simple plurals fold. All query terms must match unless no message has them all, in which case messages with the most terms rank. User messages weigh 1.3, assistant 1.0, tool calls 0.6 and tool results 0.5, so conversation outranks tool output. Searches over this machine's sessions take about 40 ms including the pull.

**MCP** (`sesh mcp`, stdio, JSON-RPC 2.0) offers message-level and session-level [tools](reference.md#mcp-tools). Its instructions tell agents to search messages and then read around them, which the verified behavior above shows works best. If the store can't be reached, it answers from the device's copy and says so.

## Server

`sesh serve` is storage for people without a bucket. It never sees plaintext: archives are readable by name, which is unguessable, and their content is sealed under tokens the server never sees; a vault's catalog is visible only to that vault's owner ([access rules](reference.md#server-access)). Someone who learned a vault id before its owner's first write could claim it and block the owner; vault ids are derived from the key and never published, so this needs a leak.

## Plugin

`plugin/` is a Claude Code plugin, and the repository is also its marketplace ([files](reference.md#plugin-files)). The `Stop` hook runs after every turn, so the launcher must never break a session: if sesh can't be installed, the hook does nothing.

Commands are the `/sesh` skill, not prompts a hook intercepts: the app shows nothing a hook prints for a blocked prompt, while a skill's output reaches the user through the model's reply. The skill itself runs nothing. Its arguments reach sesh through a `UserPromptExpansion` hook, as data: a skill's `!` command would put them in a shell, where a pasted "share token" could run commands. `sesh hook` runs only `status`, `list`, `search`, `save` and `restore`, takes everything after the command word as its argument, never as flags, and hands the model what sesh printed, errors included. That costs one model turn per command; for a restore, the turn orients the user. Only the user can run it.

Hook context is capped at about 10,000 characters, so `/sesh restore` in a cloud session hands the model 8 KB of history: the end, where the session stopped. The session is in the library, and the model reads the rest with sesh's search tools when it needs to; asked about an early decision after restoring a 2 MB session this way, it answered with one `search_messages` call.

Whatever sesh prints inside a session is kept: in the transcript, which sesh itself archives, and, in a cloud session, in the provider's event stream. So inside Claude Code, sesh never prints the vault key.

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

- Whether a prompt can be submitted at all once an account's quota is exhausted.
- Whether signed thinking blocks in a native session resume under a different account. If not, restore should drop them, as session-migrate does.
- Redacting secrets from tool output before saving. Today a session's archive holds everything its transcript does.
- How large a rendered history can be before the target model's context suffers, and how small before it misses what matters. The 100 KB default rests on one long session and one small model.
