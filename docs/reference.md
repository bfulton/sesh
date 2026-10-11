# Sesh reference

Commands, settings and formats. For how to set up and use sesh, see the [README](../README.md); for why it works this way, see [the design](design.md).

## Commands

| Command | Does |
| --- | --- |
| `sesh init [-store URL] [-key KEY]` | Creates a vault key, or uses `KEY` from another device, and records the storage URL |
| `sesh key` | Prints the vault key |
| `sesh save [-transcript FILE] [-session ID] [-dir DIR]` | Saves a session and its working copy, and prints its share token. Inside Claude Code, the current session is the default |
| `sesh sync [-q]` | Saves every local Claude Code session whose transcript changed since the last sync, conversation only |
| `sesh watch [-every 1m]` | Runs `sesh sync` repeatedly |
| `sesh restore [-dir DIR] [-budget BYTES] [-o FILE] TARGET` | Restores a session's working copy on a new branch and writes its conversation as a new Claude Code session; prints the `claude --resume` command |
| `sesh list [-n 20] [-offline]` | Lists sessions, most recently active first |
| `sesh search [-n 10] [-offline] WORDS` | Searches every session's messages and titles |
| `sesh show [-budget BYTES] [-offline] TARGET` | Prints a session's conversation |
| `sesh mcp` | Runs the MCP server on stdin and stdout |
| `sesh serve [-addr :8080] [-dir DIR]` | Runs a sesh server |
| `sesh hook` | Handles a Claude Code hook event on stdin; the plugin runs it |

`TARGET` is a share token, a session id or a unique prefix of one, or words from the session's title. `-offline` uses this device's copy of the vault without checking the store.

## Typed commands

With the plugin installed, these work as prompts in any Claude Code session. None of them makes a model call.

| Prompt | Does |
| --- | --- |
| `sesh save` | Saves now and shows the share token |
| `sesh restore TARGET` | Restores the working copy on a new branch and loads the conversation into this session |
| `sesh search WORDS` | Shows matching sessions |
| `sesh status` | Shows whether sesh is set up here |

## MCP tools

| Tool | Does |
| --- | --- |
| `search_messages` | Finds individual messages across all sessions, with each one's session id and message number |
| `read_messages` | Reads consecutive messages of one session in full; tool output is trimmed |
| `search_sessions` | Finds sessions, with a snippet of the best-matching message |
| `list_sessions` | Lists the most recently saved sessions with their titles |
| `get_session` | Reads a session's conversation, with tool calls and results trimmed to fit |

## Configuration

`~/.sesh/`, or `$SESH_HOME`, holds:

| File | Holds |
| --- | --- |
| `config` | `store=URL` |
| `key` | The vault key |
| `state.json` | Each transcript's size and modification time at its last sync |
| `hook-status.json` | The result of every hook run |
| `cache/` | This device's decrypted copy of the vault |
| `tokens/` | Share tokens for sessions saved without a vault |

| Environment variable | Effect |
| --- | --- |
| `SESH_STORE` | Storage URL; overrides `config` |
| `SESH_KEY` | Vault key; overrides `key` |
| `SESH_HOME` | Replaces `~/.sesh` |
| `SESH_BUDGET` | Size of a rendered history, in bytes (default 300000) |
| `SESH_BIN` | The plugin uses this sesh binary |
| `SESH_VERSION` | The version the plugin installs: a tag, branch or commit (default `main`) |
| `SESH_S3_PATH_STYLE` | Any value turns on path-style S3 addressing, for stores that need it |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | S3 credentials, or any other source the AWS SDK reads |
| `AWS_REGION` or `AWS_DEFAULT_REGION` | S3 region (default `auto`) |
| `AWS_ENDPOINT_URL_S3` | S3 endpoint, for stores other than AWS such as R2 or MinIO |

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
| `plugin/hooks/hooks.json` | `UserPromptSubmit` and `Stop` hooks, both running `scripts/sesh hook` |
| `plugin/.mcp.json` | The MCP server |
| `plugin/scripts/sesh` | Runs `$SESH_BIN`, else the copy it installed, else installs one with `go install`, once per `SESH_VERSION`. It never runs a `sesh` from `PATH` |
| `.claude-plugin/marketplace.json` | Makes the repository a plugin marketplace |
