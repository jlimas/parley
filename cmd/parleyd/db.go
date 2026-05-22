package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jlimas/parley/internal/toon"
)

// cmdDB dispatches parleyd db <clear>.
func cmdDB(args []string) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		dbHelp()
		return 0
	}
	handlers := map[string]func([]string) int{
		"clear": cmdDBClear,
	}
	fn, ok := handlers[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "parleyd db: unknown subcommand %q\n", args[0])
		dbHelp()
		return 2
	}
	return fn(args[1:])
}

func dbHelp() {
	fmt.Fprintf(os.Stderr, "usage: parleyd db <clear>\n\n")
	fmt.Fprintf(os.Stderr, "  clear [--yes] [--keys]   delete all posts (and optionally keys) from the database\n\n")
	fmt.Fprintf(os.Stderr, "The database path is read from PARLEY_DB (default: OS user config dir).\n")
	fmt.Fprintf(os.Stderr, "Run this while parleyd is stopped to avoid in-flight request failures.\n")
}

func cmdDBClear(args []string) int {
	fs := flag.NewFlagSet("db clear", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	yes := fs.Bool("yes", false, "confirm the destructive operation")
	keys := fs.Bool("keys", false, "also delete all API keys (requires minting new keys before next use)")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: parleyd db clear [--yes] [--keys]")
		return 2
	}

	if !*yes {
		fmt.Fprintln(os.Stderr, "error: --yes is required to confirm this destructive operation")
		if *keys {
			fmt.Fprintln(os.Stderr, "       this will permanently delete all posts AND all API keys")
		} else {
			fmt.Fprintln(os.Stderr, "       this will permanently delete all posts from the database")
		}
		fmt.Fprintln(os.Stderr, "       re-run with --yes to proceed")
		return 2
	}

	db, err := openKeyStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()

	posts, deletedKeys, err := db.Clear(*keys)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	out := toon.New(os.Stdout)
	out.KV("posts_deleted", posts)
	if *keys {
		out.KV("keys_deleted", deletedKeys)
	}
	out.KV("status", "cleared")
	hints := []string{"Restart parleyd to begin fresh"}
	if *keys {
		hints = append(hints, "Run `parleyd keys create --description \"...\"` to mint a new API key before starting")
	}
	out.Help(hints...)
	return 0
}
