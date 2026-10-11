# Sesh

Sesh keeps a private copy of every AI agent session you have, so you can pick any of them up anywhere and search across all of them.

- **Keep.** Every Claude Code session is saved as you work: the conversation, and the uncommitted changes in your repo. Everything is encrypted on your device; storage only ever holds ciphertext.
- **Continue.** Carry on any session somewhere else: another account, another machine, a cloud session. It comes back with its full history and its unfinished work.
- **Remember.** Search across all your sessions, and let any session pull context from the others ("what did we decide about the token format?").

## Install

In Claude Code:

```
/plugin install sesh --marketplace bfulton/sesh
```

The plugin installs the `sesh` binary on first use (it needs Go 1.24+), or uses `sesh` from your PATH. To install the binary yourself:

```
go install github.com/bfulton/sesh/cmd/sesh@main
```

That puts `sesh` in `~/go/bin`; add it to your PATH, or use the full path below.

## Set up

```
sesh init -store s3://my-bucket/sesh      # or file:///path, or https://your-sesh-server
```

This creates your **vault key**, a 26-character string that is the only way to read your sessions. Keep it like a password.

On another computer:

```
sesh init -key <vault key> -store <same storage URL>
```

In Claude Code on the web, set two environment variables in your cloud environment: `SESH_KEY` (your vault key) and `SESH_STORE` (the storage URL). For an S3-compatible bucket, also set `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and, for R2 or MinIO, `AWS_ENDPOINT_URL_S3`.

## Use it

With the plugin, sessions are kept automatically after every turn. In any Claude Code session you can also type these as prompts; none of them uses a model call:

| Type | What happens |
| --- | --- |
| `sesh save` | Saves now and shows the session's share token |
| `sesh restore <token, session id or title words>` | Restores that session's uncommitted work on a new branch and loads its conversation into this session |
| `sesh search <words>` | Shows matching sessions |
| `sesh status` | Shows whether sesh is set up here |

Agents can search your sessions on their own through the plugin's MCP server (`search_messages`, `read_messages`, `search_sessions`, `list_sessions`, `get_session`).

From a terminal:

```
sesh sync                    # save every local Claude Code session that changed
sesh watch                   # keep doing that, every minute
sesh list                    # your sessions, most recent first
sesh search token alphabet   # search them all
sesh show <id or title>      # read one
sesh restore <id or token>   # restore its work here; prints a `claude --resume` command
                             # that continues it with its real turns
sesh mcp                     # MCP server for any agent:
                             #   claude mcp add -s user sesh -- ~/go/bin/sesh mcp
```

## Sharing a session

Every session has a **share token**. Anyone with it, and access to your storage, can restore that one session and nothing else; with `sesh serve`, they need only the token. Treat it like a password for that session: it includes everything in the conversation, including any secrets that appeared in tool output.

## Storage

Sesh works with storage you choose:

- **An S3-compatible bucket** you own: AWS S3, Cloudflare R2, MinIO, …
- **A folder**, `file:///path`, e.g. one that a sync tool replicates.
- **A sesh server**: `sesh serve` runs one. It stores only ciphertext, lets anyone fetch a session archive by its unguessable name, and keeps each vault's catalog private to that vault. Run it behind TLS.

## How it works

See [docs/design.md](docs/design.md).

## License

MIT
