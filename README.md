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

The plugin installs the `sesh` binary on first use, which needs Go 1.24 or later. To use a binary you installed or built yourself, set `SESH_BIN` to its path; the plugin doesn't use `sesh` from your `PATH`, since other tools share the name. To install the binary yourself:

```
go install github.com/bfulton/sesh/cmd/sesh@main
```

That puts `sesh` in `~/go/bin`. Add that folder to your `PATH`, or use the full path.

## Set up

1. Choose storage and create your vault key:

   ```
   sesh init -store s3://my-bucket/sesh
   ```

   For a folder or a sesh server instead of a bucket, see [the storage reference](docs/reference.md#storage). The command prints your **vault key**, a 26-character string that is the only way to read your sessions. Keep it like a password.

2. On each other computer, use the same key and storage:

   ```
   sesh init -key VAULT_KEY -store STORAGE_URL
   ```

3. For Claude Code on the web, set `SESH_KEY` and `SESH_STORE` in your cloud environment's variables. For an S3-compatible bucket, also set its credentials; see [the configuration reference](docs/reference.md#configuration).

4. Optional: let any Claude Code session search your sessions without the plugin:

   ```
   claude mcp add -s user sesh -- ~/go/bin/sesh mcp
   ```

## Use sesh

With the plugin, sessions are kept automatically after every turn. To keep the sessions on a computer without the plugin, run `sesh sync`, or `sesh watch` to keep syncing.

To continue a session in a terminal:

1. In the folder to continue it in, run:

   ```
   sesh restore SESSION
   ```

   `SESSION` is a session id, words from its title, or a share token. Sesh restores the session's uncommitted work on a new branch and prints a `claude --resume` command.

2. Run the printed command. The session continues with its real turns.

Inside a Claude Code session, type `sesh restore SESSION` as a prompt instead; the conversation loads into the current session.

To find something from a past session, run `sesh search WORDS`, or ask an agent that has the sesh MCP server.

For every command, see [the reference](docs/reference.md#commands).

## Share a session

Every session has a **share token**: `sesh save` prints it. Anyone with the token and access to your storage can restore that one session and nothing else; with a sesh server, they need only the token. Treat it like a password for that session: it includes everything in the conversation, including any secrets that appeared in tool output.

## Run a sesh server

If you don't have a bucket, `sesh serve` provides storage that only ever sees ciphertext. Run it behind TLS. For its access rules, see [the reference](docs/reference.md#server-access).

## Develop

Build and test:

```
go build ./...
go test ./...
```

CI also checks formatting, runs `go vet` and the race detector on Linux and macOS, builds for Linux, macOS and Windows, and runs `shellcheck` on the plugin launcher; see [the workflow](.github/workflows/ci.yml).

To try a change in Claude Code, build sesh and point the plugin at it with `SESH_BIN`, or load the plugin from your checkout with `claude --plugin-dir plugin`.

## Learn more

- [Reference](docs/reference.md): commands, settings and formats.
- [Design](docs/design.md): how sesh works and why.
- [Contributing](CONTRIBUTING.md), [changelog](CHANGELOG.md) and [security policy](SECURITY.md).

## License

MIT
