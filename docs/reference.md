# Sesh reference

Commands, settings and formats. For how to set up and use sesh, see the [README](../README.md); for why it works this way, see [the design](design.md).

## Commands

| Command | Does |
| --- | --- |
| `sesh init [-store URL] [-key KEY]` | Creates a vault key, or uses `KEY` from another device, and records the storage URL. Inside Claude Code it doesn't print the key; in a cloud session it refuses |
| `sesh key` | Prints the vault key. Refuses inside Claude Code, whose transcript would keep it |
| `sesh status` | Shows the vault, the storage URL with any credentials hidden, whether redaction is on, and the last hook run |
| `sesh version` | Prints the release this binary was built for, or `dev` |
| `sesh save [-transcript FILE] [-session ID] [-dir DIR]` | Saves a session and its working copy, and prints its share token. Inside Claude Code, the current session is the default |
| `sesh sync [-q]` | Saves every local Claude Code session whose transcript changed since the last sync, conversation only |
| `sesh watch [-every 1m]` | Runs `sesh sync` repeatedly |
| `sesh restore [-dir DIR] [-budget BYTES] [-o FILE] TARGET` | Restores a session's working copy on a new branch and writes its conversation as a new Claude Code session; prints the `claude --resume` command. In a cloud session it prints the rendered history instead. Either way the session joins this device's library |
| `sesh list [-n 20] [-offline]` | Lists sessions, most recently active first |
| `sesh search [-n 10] [-offline] WORDS` | Searches every session's messages and titles |
| `sesh show [-budget BYTES] [-offline] TARGET` | Prints a session's conversation |
| `sesh mcp` | Runs the MCP server on stdin and stdout |
| `sesh serve [-addr :8080] [-dir DIR]` | Runs a sesh server |
| `sesh hook` | Handles a Claude Code hook event on stdin: `Stop` saves the session, and `UserPromptExpansion` runs a `/sesh` command. The plugin runs it |

`TARGET` is a share token, a session id or a unique prefix of one, or words from the session's title. `-offline` uses this device's copy of the vault without checking the store.

## The /sesh command

With the plugin installed, `/sesh COMMAND` runs a sesh command in any Claude Code session, and Claude shows what it printed, errors included. Each use is one model turn. Claude never runs it on its own, and nothing typed after `/sesh` reaches a shell.

| Command | Does |
| --- | --- |
| `/sesh status` | Shows whether sesh is set up here |
| `/sesh list` | Lists your sessions |
| `/sesh search WORDS` | Shows matching sessions |
| `/sesh save` | Saves now and shows the share token |
| `/sesh restore TARGET` | Restores the working copy on a new branch. In a terminal session, prints a `claude --resume` command for the real turns; in a cloud session, Claude says where the session stopped from the end of its history, and the whole history (`SESH_BUDGET`, 100 KB by default) joins this session when that turn ends |

`/sesh` alone shows the status. `TARGET` and `WORDS` are everything after the command word.

## MCP tools

| Tool | Does |
| --- | --- |
| `search_messages` | Finds individual messages across all sessions, with each one's session id and message number |
| `read_messages` | Reads consecutive messages of one session in full; tool output is trimmed |
| `list_messages` | Lists every message of one kind, by default the user's, across all sessions or those chosen by session or start date, with the total count, a page at a time |
| `search_sessions` | Finds sessions, with a snippet of the best-matching message |
| `list_sessions` | Lists the most recently saved sessions with their titles |
| `get_session` | Reads a session's conversation, with tool calls and results trimmed to fit |

## Configuration

`~/.sesh/`, or `$SESH_HOME`, holds:

| File | Holds |
| --- | --- |
| `config` | `store=URL`, and `redact=off` to save secrets as they are |
| `key` | The vault key |
| `state.json` | Each transcript's size and modification time at its last sync |
| `hook-status.json` | The result of every hook run |
| `cache/` | This device's decrypted copy of the vault |
| `scanned/` | For each session, hashes of the content already scanned for secrets and found clean; never content |
| `tokens/` | Share tokens for sessions saved without a vault |

| Environment variable | Effect |
| --- | --- |
| `SESH_STORE` | Storage URL; overrides `config` |
| `SESH_KEY` | Vault key; overrides `key` |
| `SESH_HOME` | Replaces `~/.sesh` |
| `SESH_BUDGET` | Size of a rendered history, in bytes (default 100000) |
| `SESH_REDACT` | `off` saves secrets as they are; overrides `config` |
| `SESH_BIN` | The plugin uses this sesh binary |
| `SESH_VERSION` | The version the plugin installs: a release tag, downloaded, or any tag, branch or commit, built with `go install` (default: the plugin's release) |
| `SESH_RELEASES` | Where the plugin downloads releases from (default `https://github.com/bfulton/sesh/releases/download`) |
| `SESH_S3_PATH_STYLE` | Any value turns on path-style S3 addressing, for stores that need it |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | S3 credentials, or any other source the AWS SDK reads |
| `AWS_REGION` or `AWS_DEFAULT_REGION` | S3 region (default `auto`) |
| `AWS_ENDPOINT_URL_S3` | S3 endpoint, for stores other than AWS such as R2 or MinIO |

## Secrets

Before a save is encrypted, sesh replaces each secret it finds with a placeholder that names the rule that found it, such as `[REDACTED:github-pat]`. It finds secrets with the 222 default rules of [gitleaks](https://github.com/gitleaks/gitleaks) v8.30.1, which cover cloud, source-control, payment and AI provider keys, private keys, JWTs and generic `key = value` assignments, including inside base64, hex and percent encoding.

| Part of a save | Scanned |
| --- | --- |
| Transcript and subagent transcripts | Every JSON string, decoded: object keys, and values after their key. Base64 image and document data, which is compressed binary, is skipped |
| Large tool outputs and memory files | As text; files that aren't UTF-8 are skipped |
| Working copy | Lines the patches add. Lines a patch keeps or removes are in the base commit, which a restore needs anyway, and changing them would stop the patch from applying. Binary patches are skipped |

The manifest records how many placeholders each file got. `sesh save` and the hook say how many; `sesh restore` names the working-copy files that hold placeholders, since those files are restored with the placeholders instead of the secrets.

A save also never keeps credentials in the remote URL it records, such as a token in `https://x-access-token:TOKEN@github.com/…`, whatever the setting.

Rules can match text that isn't a secret, such as a test fixture with a private key. To save everything as it is, set `redact=off` in `config` or `SESH_REDACT=off`.

The first save of a session scans all of it, at roughly 1 MB a second. `~/.sesh/scanned/` remembers what was clean, so later saves scan only what changed. The `Stop` hook spends at most 3 minutes scanning, and `/sesh save` 2 minutes; a save out of time uploads nothing, keeps what it scanned, and the next save carries on.

## Storage

| URL | Store |
| --- | --- |
| `file:///path` | A directory |
| `s3://bucket/prefix` | An S3-compatible bucket |
| `https://host` | A sesh server |

Objects are kept under `sesh/`:

```
v1/s/<archive id>               one session's archive, sealed under its share token
v1/c/<vault id>/<entry id>      one catalog entry, sealed under the catalog key
```

A **catalog entry** is JSON: session id, source, title, working directory, repo, branch, start, last activity, save time, message count, transcript size, share token and host. It is sealed with AES-256-GCM, with its object name as associated data.

An **archive** is a 12-byte random nonce followed by the AES-256-GCM ciphertext of a gzipped tar:

```
manifest.json                   format version, source, session id, repo, branch, base commit,
                                save time, changed and deleted paths, memory files,
                                the session folder's original path and files
transcript.jsonl                the native transcript, verbatim
worktree/patches/<path>.patch   one binary diff per changed path, against base
memory/<path>                   the session's Claude Code project memory folder, if any
session/<path>                  the session folder's tool-results/ and subagents/, up to 64 MB
```

## Keys and tokens

The vault key is 128 random bits, written as 26 characters of lowercase Crockford base32 (`0-9a-z` without `i l o u`). Decoding ignores case and maps `i` and `l` to `1`, and `o` to `0`.

HKDF-SHA256 over the vault key derives:

| Output | Info string | Use |
| --- | --- | --- |
| Share token for a session | `sesh session v1:<session id>` | Opens that session's archive |
| Vault id (16 bytes, hex) | `sesh vault id v1` | Names the vault's catalog |
| Catalog key (32 bytes) | `sesh catalog key v1` | Seals catalog entries |
| Entry id (16 bytes, hex) | `sesh entry v1:<session id>` | Names a session's catalog entry |
| Server token (32 bytes, hex) | `sesh server access v1` | Bearer token for a sesh server |

HKDF over a share token derives its archive's encryption key (`sesh key v1`) and archive id (`sesh id v1`).

## Server access

| Name | Read | Write | List |
| --- | --- | --- | --- |
| `v1/s/<id>` | Anyone | The bearer that first wrote it | No one |
| `v1/c/<vault>/…` | The vault's owner; others get 404 | The vault's owner | The vault's owner; others get an empty list |

A vault's owner is the first bearer to write in it. Bearers are stored as SHA-256 hashes. Objects are capped at 256 MB.

## Plugin files

| File | Contents |
| --- | --- |
| `plugin/hooks/hooks.json` | The `Stop` hook and the `/sesh` command's `UserPromptExpansion` hook, both running `scripts/sesh hook` |
| `plugin/skills/sesh/SKILL.md` | The `/sesh` command |
| `plugin/.mcp.json` | The MCP server |
| `plugin/scripts/sesh` | Runs `$SESH_BIN`, else the copy it installed, else installs one, once per `SESH_VERSION`: it downloads the release binary for this OS and CPU and checks it against the release's `SHA256SUMS`, and falls back to `go install`. It never runs a `sesh` from `PATH` |
| `.claude-plugin/marketplace.json` | Makes the repository a plugin marketplace |
