// Command sesh keeps your AI agent sessions in storage you own, so you can
// continue any of them anywhere and search across all of them.
//
//	sesh init                  create your vault key and set up storage
//	sesh save                  save this session; print its share token
//	sesh restore <token|id>    continue a saved session here
//	sesh sync / watch          save every local session that changed
//	sesh list / search / show  browse and search your sessions
//	sesh mcp                   serve your sessions to agents over MCP
//	sesh serve                 run a storage server that sees only ciphertext
//	sesh hook                  Claude Code hook entry point
package main

import (
	"fmt"
	"os"
)

const usageText = `sesh keeps your agent sessions in storage you own.

Set up:
  sesh init [-store URL]        create a vault key and choose storage
  sesh key                      print your vault key (for other devices)
  sesh status                   show the vault, storage and last hook run

Keep:
  sesh save                     save the current session; print its share token
  sesh sync                     save every local session that changed
  sesh watch [-every 1m]        keep syncing

Continue:
  sesh restore <token|id|title> restore a session's work here and write it as
                                a session that claude --resume continues

Remember:
  sesh list [-n 20]             your sessions, newest first
  sesh search <query>           search across all sessions
  sesh show <id|title>          print a session's conversation
  sesh mcp                      MCP server: lets any agent search your sessions

Run a server:
  sesh serve [-addr :8080] [-dir path]   storage that only ever sees ciphertext

In Claude Code, the plugin saves each session as it goes and accepts
"sesh save", "sesh restore", "sesh search" and "sesh status" typed as prompts.

Storage is a URL: file:///path, s3://bucket/prefix, or https://your-sesh-server.
Settings come from ~/.sesh/config, overridden by SESH_STORE and SESH_KEY.`

// subcommands maps each subcommand to its function.
var subcommands = map[string]func([]string) error{
	"init":    cmdInit,
	"key":     cmdKey,
	"status":  cmdStatus,
	"save":    cmdSave,
	"restore": cmdRestore,
	"sync":    cmdSync,
	"watch":   cmdWatch,
	"list":    cmdList,
	"search":  cmdSearch,
	"show":    cmdShow,
	"mcp":     cmdMCP,
	"serve":   cmdServe,
	"hook":    func([]string) error { return cmdHook(os.Stdin, os.Stdout) },
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	run, ok := subcommands[os.Args[1]]
	if !ok {
		usage()
	}
	if err := run(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "sesh:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, usageText)
	os.Exit(2)
}
