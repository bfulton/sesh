# Sesh design

Sesh moves an agent session between sessions, accounts and machines. `sesh save` packs the conversation and the working copy into one encrypted object in your own bucket and prints a token. `sesh restore <token>` in another session unpacks it: the working copy comes back exactly, and the conversation comes back into the model's context.

Both commands are typed as ordinary prompts. A `UserPromptSubmit` hook intercepts them and stops the turn, so neither costs a model call.

## What we verified

All checks ran on 2026-10-10 in Claude Code cloud sessions (CLI 2.1.296).

| Finding | Evidence |
| --- | --- |
| A session can read its own transcript | `~/.claude/projects/<cwd>/<session-id>.jsonl`, plain file reads |
| Repo-level hooks fire in cloud sessions | A repo `Stop` hook ran in a cloud session; a repo `UserPromptSubmit` hook ran `sesh restore` in another |
| Writing a hosted session's transcript file changes nothing | Appended records never reached the server or the running model |
| A stopped turn costs nothing | `continue: false` from `UserPromptSubmit`: `num_turns: 0`, session cost unchanged |
| A stopped turn's `stopReason` stays in the model's context | Hosted session recalled a codeword carried only in `stopReason` |
| `stopReason` has no practical size cap | 200,000 characters stored whole; facts at start, middle and end recalled |
| `decision: "block"` does not reach the model's context | A blocked prompt's text was not recalled later |
| The app does not display `stopReason` | The restore turn showed nothing in the Claude app |
| The local CLI resumes a transcript it didn't write | A hand-written session file plus `claude --resume` recalled its codeword |
| A restored native session keeps real turns, tool results and thinking | After `sesh restore` from a shell, `claude --resume` recalled a fact seen only in a tool result, and a session with signed thinking blocks resumed cleanly (same account) |

## Commands

```
sesh save                 # capture, encrypt, upload; print the token
sesh restore <token>      # download, decrypt; restore the working copy; load the conversation
```

Both run from the hook (typed as prompts in a session) or directly from a shell.

## Token

- 128 random bits, written as 26 characters of lowercase Crockford base32: `0-9a-z` without `i l o u`.
- Letters and digits only, so a double-tap selects the whole token on a phone.
- Decoding ignores case and maps `i l`→`1` and `o`→`0`.
- One token per session. Each save overwrites that session's object, so a token always restores the latest save.
- The token is the only secret: it locates the object and decrypts it. Anyone holding it can read the session.

From the token's 16 bytes, HKDF-SHA256 derives:

| Output | Use |
| --- | --- |
| 32-byte key (`info = "sesh key v1"`) | AES-256-GCM encryption |
| 16-byte id (`info = "sesh id v1"`) | Object name `sesh/v1/<hex id>`; reveals nothing about the key |

## Archive

One object per session: a 12-byte random nonce followed by the AES-256-GCM ciphertext of a gzipped tar.

```
manifest.json        # format version, source, session id, repo, branch, base commit, timestamps
transcript.jsonl     # the source session's native transcript, verbatim
worktree/patches/<path>.patch   # one binary diff per changed path, against base
```

`manifest.json` also lists deleted paths.

### Working copy

Capture:
1. Base is the newest commit reachable from a remote-tracking branch (`git merge-base HEAD @{upstream}`, falling back to the remote default branch). The target can fetch it. Unpushed commits are carried as file content, not history.
2. Copy the index to a private index file with `cp -p`, then `git add -A` into it. The user's staging area is untouched. The `-p` matters: a fresh mtime makes git miss same-second, same-size edits.
3. For each changed path, `git diff --cached --binary` against base becomes one patch. Deleted paths are listed in the manifest, not written as patches.

Restore: fetch and check out base on a new branch, apply every patch, remove every deleted path. A dirty target working copy is refused.

## Restoring the conversation

Real turns can only reach a session started from a session file: a running CLI never re-reads its transcript, and a hosted session's transcript lives on the provider's server. So restore picks the form from where it runs, with no option to choose:

| Where `sesh restore` runs | Result |
| --- | --- |
| A shell | A native session file for the target directory; prints `claude --resume <id>` |
| The hook, in a local CLI session | The rendered history as `stopReason`, so this session continues now, plus a native session file for real turns |
| The hook, in a hosted session (`CLAUDE_CODE_REMOTE=true`) | The rendered history as `stopReason` only |

The native file is the saved transcript with every record's `sessionId` and `cwd` rewritten; content, uuids and the `parentUuid` chain are unchanged. It goes in `~/.claude/projects/<cwd>/`, where `<cwd>` has every character other than an ASCII letter or digit replaced by `-`.

### Rendered history

In the hook, the transcript is rendered as text and returned as `stopReason` with `continue: false`. The model sees it on every later turn. The app does not show it, so the hook also prints a short visible confirmation.

Rendering rules:
- Frame it as a quoted record of past turns, not instructions, so it isn't read as a prompt injection.
- Keep user and assistant text whole. Truncate long tool inputs and results, keeping their start and end.
- Drop thinking blocks and harness records (attachments, hook progress, cost and title records).
- Fit a character budget: recent turns verbatim, older tool output trimmed first.

## Storage

The bucket is any S3-compatible store you own. The store holds only ciphertext under unguessable names. Save needs write access; restore needs read access to one object. Whether the bucket allows anonymous reads (so restore needs only the token) is a deployment choice.

A local directory store exists for tests and offline use.

## Out of scope

- Writing into a hosted session's transcript, or its internal ingress endpoint.
- Automatic per-turn saves. Save is manual.
- Codex. Its hooks and rollout format are unverified.

## Related work

- **[ctxmv](https://github.com/Ryu0118/ctxmv)** (v0.7.0) moves sessions between local agents (Claude Code, Codex, Cursor, Kimi, Copilot CLI) by writing the target's native session file. It works on one machine only, carries no working copy, and keeps only user and assistant text: tool calls, tool results, thinking and images are dropped. On one real session that kept 96,000 characters and dropped 147 tool calls and 146 results (about 338,000 characters). Its code documents Claude Code's resume requirements, and its parsing library [AgentSessions](https://github.com/Ryu0118/AgentSessions) reads the other agents' formats.
- **[Unified Agent Session Specification](https://github.github.com/gh-aw/specs/unified-agent-session-specification/)** (GitHub Agentic Workflows, draft v1.7.0) defines neutral session events with native passthrough, for logging and analysis across Claude, Codex, Copilot, Gemini and others. It has no working copy and no resume semantics. It is the likely neutral format if sesh adds other agents; Claude-to-Claude restore keeps the native transcript.

## Open questions

- Which hook output channel the Claude app displays: block `reason`, `systemMessage`, or neither.
- Whether a prompt can be submitted at all once an account's quota is exhausted.
- How large a rendered history can be before the target model's context suffers.
- Whether signed thinking blocks in a native session resume under a different account.
