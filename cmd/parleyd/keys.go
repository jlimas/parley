package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jlimas/parley/internal/store"
	"github.com/jlimas/parley/internal/toon"
)

func cmdKeys(args []string) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		keysHelp()
		return 0
	}
	handlers := map[string]func([]string) int{
		"create": cmdKeysCreate,
		"list":   cmdKeysList,
		"revoke": cmdKeysRevoke,
	}
	fn, ok := handlers[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "parleyd keys: unknown subcommand %q\n", args[0])
		keysHelp()
		return 2
	}
	return fn(args[1:])
}

func keysHelp() {
	fmt.Fprintf(os.Stderr, "usage: parleyd keys <create|list|revoke>\n\n")
	fmt.Fprintf(os.Stderr, "  create --tenant <id> --agent <name>   mint a new API key (printed once)\n")
	fmt.Fprintf(os.Stderr, "  list [--tenant <id>]                  show keys (all tenants if omitted)\n")
	fmt.Fprintf(os.Stderr, "  revoke <id>                           revoke a key by ID\n\n")
	fmt.Fprintf(os.Stderr, "The key store is read from PARLEY_DB (default: OS user config dir).\n")
}

func cmdKeysCreate(args []string) int {
	fs := flag.NewFlagSet("keys create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tenantID := fs.String("tenant", "", "tenant ID this key belongs to")
	agent := fs.String("agent", "", "agent name this key authenticates as (e.g. \"alice\")")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: parleyd keys create --tenant <id> --agent <name>")
		return 2
	}
	if *tenantID == "" {
		fmt.Fprintln(os.Stderr, "error: --tenant is required")
		fmt.Fprintln(os.Stderr, "usage: parleyd keys create --tenant <id> --agent <name>")
		fmt.Fprintln(os.Stderr, "hint:  run `parleyd tenants list` to find tenant IDs")
		return 2
	}
	if *agent == "" {
		fmt.Fprintln(os.Stderr, "error: --agent is required")
		fmt.Fprintln(os.Stderr, "usage: parleyd keys create --tenant <id> --agent <name>")
		return 2
	}

	db, err := openKeyStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()

	exists, err := db.TenantExists(*tenantID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: check tenant: %v\n", err)
		return 1
	}
	if !exists {
		fmt.Fprintf(os.Stderr, "error: tenant %q not found\n", *tenantID)
		fmt.Fprintln(os.Stderr, "hint:  run `parleyd tenants list` to find tenant IDs")
		return 1
	}

	plaintext, rec, err := db.CreateKey(*tenantID, *agent)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: create key: %v\n", err)
		return 1
	}

	out := toon.New(os.Stdout)
	out.KV("id", rec.ID)
	out.KV("tenant_id", rec.TenantID)
	out.KV("agent", rec.Agent)
	out.KV("created_at", rec.CreatedAt.UTC().Format(time.RFC3339))
	out.KV("key", plaintext)
	out.KV("notice", "store this key now — it will not be shown again")
	out.Help(
		"Share the key out-of-band with the agent owner",
		"The agent owner runs: parley config key <key>",
		"Run `parleyd keys list` to see all active keys",
	)
	return 0
}

func cmdKeysList(args []string) int {
	fs := flag.NewFlagSet("keys list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tenantID := fs.String("tenant", "", "filter by tenant ID (omit for all tenants)")
	_ = fs.Parse(args)

	db, err := openKeyStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()

	out := toon.New(os.Stdout)

	if *tenantID != "" {
		recs, err := db.ListKeys(*tenantID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: list keys: %v\n", err)
			return 1
		}
		if len(recs) == 0 {
			out.KV("keys", "none")
			out.Help(fmt.Sprintf("Run `parleyd keys create --tenant %s --agent <name>` to mint a key", *tenantID))
			return 0
		}
		now := time.Now()
		rows := make([][]any, len(recs))
		for i, k := range recs {
			revoked := "-"
			if !k.RevokedAt.IsZero() {
				revoked = humanAge(now, k.RevokedAt)
			}
			rows[i] = []any{k.ID, k.Agent, humanAge(now, k.CreatedAt), revoked}
		}
		out.Table("keys", []string{"id", "agent", "created", "revoked"}, rows)
	} else {
		recs, err := db.ListAllKeys()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: list keys: %v\n", err)
			return 1
		}
		if len(recs) == 0 {
			out.KV("keys", "none")
			out.Help("Run `parleyd tenants create --name \"...\"` then `parleyd keys create --tenant <id> --agent <name>`")
			return 0
		}
		now := time.Now()
		rows := make([][]any, len(recs))
		for i, k := range recs {
			revoked := "-"
			if !k.RevokedAt.IsZero() {
				revoked = humanAge(now, k.RevokedAt)
			}
			rows[i] = []any{k.ID, k.TenantID, k.Agent, humanAge(now, k.CreatedAt), revoked}
		}
		out.Table("keys", []string{"id", "tenant_id", "agent", "created", "revoked"}, rows)
	}
	out.Help("Run `parleyd keys revoke <id>` to revoke a key")
	return 0
}

func cmdKeysRevoke(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: parleyd keys revoke <id>")
		return 2
	}
	id := args[0]

	db, err := openKeyStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()

	found, err := db.RevokeKey(id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: revoke key: %v\n", err)
		return 1
	}
	out := toon.New(os.Stdout)
	if !found {
		out.KV("status", fmt.Sprintf("key %s not found or already revoked", id))
		return 1
	}
	out.KV("id", id)
	out.KV("status", "revoked")
	out.Help("Clients using this key will receive 401 on the next request")
	return 0
}

func openKeyStore() (*store.Store, error) {
	dsn, err := resolveDSN(os.Getenv("PARLEY_DB"))
	if err != nil {
		return nil, fmt.Errorf("resolve db path: %w", err)
	}
	return store.Open(dsn)
}

func humanAge(now, t time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hr ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}
