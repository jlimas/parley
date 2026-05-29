package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jlimas/parley/internal/toon"
)

func cmdTenants(args []string) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		tenantsHelp()
		return 0
	}
	handlers := map[string]func([]string) int{
		"create": cmdTenantsCreate,
		"list":   cmdTenantsList,
	}
	fn, ok := handlers[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "parleyd tenants: unknown subcommand %q\n", args[0])
		tenantsHelp()
		return 2
	}
	return fn(args[1:])
}

func tenantsHelp() {
	fmt.Fprintf(os.Stderr, "usage: parleyd tenants <create|list>\n\n")
	fmt.Fprintf(os.Stderr, "  create --name \"...\"   create a new tenant (returns tenant ID)\n")
	fmt.Fprintf(os.Stderr, "  list                  show all tenants\n\n")
	fmt.Fprintf(os.Stderr, "The tenant ID is used when minting keys: parleyd keys create --tenant <id>\n")
}

func cmdTenantsCreate(args []string) int {
	fs := flag.NewFlagSet("tenants create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "display name for this tenant (e.g. \"Acme Corp\")")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: parleyd tenants create --name \"<display name>\"")
		return 2
	}
	if *name == "" {
		fmt.Fprintln(os.Stderr, "error: --name is required")
		fmt.Fprintln(os.Stderr, "usage: parleyd tenants create --name \"<display name>\"")
		return 2
	}

	db, err := openKeyStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()

	rec, err := db.CreateTenant(*name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: create tenant: %v\n", err)
		return 1
	}

	out := toon.New(os.Stdout)
	out.KV("id", rec.ID)
	out.KV("name", rec.Name)
	out.KV("created_at", rec.CreatedAt.UTC().Format(time.RFC3339))
	out.Help(
		fmt.Sprintf("Mint a key for this tenant: parleyd keys create --tenant %s", rec.ID),
		"Run `parleyd tenants list` to see all tenants",
	)
	return 0
}

func cmdTenantsList(args []string) int {
	_ = args
	db, err := openKeyStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()

	tenants, err := db.ListTenants()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: list tenants: %v\n", err)
		return 1
	}

	out := toon.New(os.Stdout)
	if len(tenants) == 0 {
		out.KV("tenants", "none")
		out.Help("Run `parleyd tenants create --name \"...\"` to create the first tenant")
		return 0
	}

	now := time.Now()
	rows := make([][]any, len(tenants))
	for i, t := range tenants {
		rows[i] = []any{t.ID, t.Name, humanAge(now, t.CreatedAt)}
	}
	out.Table("tenants", []string{"id", "name", "created"}, rows)
	out.Help(
		"Mint a key: parleyd keys create --tenant <id> [--description \"...\"]",
		"Run `parleyd keys list` to see all keys",
	)
	return 0
}
