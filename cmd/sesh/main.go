// Command sesh keeps your AI agent sessions in storage you own, so you can
// continue any of them anywhere and search across all of them.
//
//	sesh init                  create your vault key and set up storage
//	sesh env                   print the settings of a cloud environment
//	sesh status                show the vault, storage and last hook run
//	sesh version               print this binary's version
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
	"runtime/debug"
)

const usageText = `sesh keeps your agent sessions in storage you own.

Set up:
  sesh init [VAULT]             create a vault key, with storage on this device
  sesh init -aws BUCKET[/PREFIX] [VAULT]
                                ... with storage in S3: makes the bucket if
                                needed, and an IAM user for the vault alone
  sesh init -store URL [VAULT]  ... with storage you already have
  sesh env                      print the variables for a cloud environment
  sesh key                      print your vault key
  sesh status                   show the vault, storage and last hook run
  sesh version                  print this binary's version

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

In Claude Code, the plugin saves each session after every turn, and
/sesh <command> runs these commands from any session.

Storage is a URL: file:///path, s3://bucket/prefix, or https://your-sesh-server.
Each vault's settings are in ~/.sesh/VAULT; the vault is SESH_VAULT, else
"default". SESH_KEY, SESH_STORE and the other variables sesh env prints
override them.`

// subcommands maps each subcommand to its function.
var subcommands = map[string]func([]string) error{
	"init":    cmdInit,
	"key":     cmdKey,
	"env":     cmdEnv,
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
	"version": func([]string) error { fmt.Println(versionString()); return nil },
}

// version is the release this binary was built for, set by the release
// build with -ldflags "-X main.version=v0.3.0".
var version string

// versionString is the release this binary was built for: the one stamped
// at build time, else the module version go install recorded, else "dev".
func versionString() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	run, ok := subcommands[os.Args[1]]
	if !ok {
		usage()
	}
	if err := checkVaultName(vaultName()); err != nil {
		fmt.Fprintln(os.Stderr, "sesh: SESH_VAULT:", err)
		os.Exit(1)
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
