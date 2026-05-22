// Command parleyd is the parley broker server.
//
// It accepts events POSTed by clients and fans them out to subscribers over
// Server-Sent Events. State is persisted to a SQL database; restarting the
// broker preserves the board.
//
// Subcommands (do not start the server):
//
//	parleyd keys create --description "..."   mint a new API key
//	parleyd keys list                         list all keys
//	parleyd keys revoke <id>                  revoke a key by ID
//	parleyd db clear [--yes] [--keys]         delete all posts (and optionally keys)
//	parleyd healthcheck                       exit 0 if /healthz is reachable, 1 otherwise
package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/jlimas/parley/internal/server"
	"github.com/jlimas/parley/internal/store"
)

func main() {
	// Subcommands operate on the store directly without starting the HTTP server.
	// They must be run as a separate invocation (stop parleyd first for db clear).
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--help", "-h", "help":
			topLevelHelp()
			os.Exit(0)
		case "keys":
			os.Exit(cmdKeys(os.Args[2:]))
		case "db":
			os.Exit(cmdDB(os.Args[2:]))
		case "healthcheck":
			os.Exit(cmdHealthcheck())
		}
	}

	addr := os.Getenv("PARLEY_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	dsn, err := resolveDSN(os.Getenv("PARLEY_DB"))
	if err != nil {
		log.Fatalf("parleyd: %v", err)
	}
	db, err := store.Open(dsn)
	if err != nil {
		log.Fatalf("parleyd: %v", err)
	}
	defer db.Close()

	initial, err := db.Load()
	if err != nil {
		log.Fatalf("parleyd: load history: %v", err)
	}
	log.Printf("parleyd: db=%s replayed=%d", safeDB(dsn), len(initial))

	srv := server.New(db, initial, server.Options{
		Keys:    db,
		Tracker: db,
	})
	log.Printf("parleyd listening on %s", addr)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		log.Fatalf("parleyd: %v", err)
	}
}

func topLevelHelp() {
	fmt.Fprintf(os.Stderr, "usage: parleyd [--help] [<subcommand>]\n\n")
	fmt.Fprintf(os.Stderr, "With no subcommand, parleyd starts the broker server.\n\n")
	fmt.Fprintf(os.Stderr, "environment variables:\n")
	fmt.Fprintf(os.Stderr, "  PARLEY_ADDR   listen address (default :8080)\n")
	fmt.Fprintf(os.Stderr, "  PARLEY_DB     database path or DSN (default: OS user config dir)\n\n")
	fmt.Fprintf(os.Stderr, "subcommands (do not start the server):\n")
	fmt.Fprintf(os.Stderr, "  keys create --description \"...\"   mint a new API key\n")
	fmt.Fprintf(os.Stderr, "  keys list                         list all API keys\n")
	fmt.Fprintf(os.Stderr, "  keys revoke <id>                  revoke a key by ID\n")
	fmt.Fprintf(os.Stderr, "  db clear [--yes] [--keys]         delete all posts (and optionally keys)\n")
	fmt.Fprintf(os.Stderr, "  healthcheck                       exit 0 if /healthz is reachable, 1 otherwise\n\n")
	fmt.Fprintf(os.Stderr, "Run `parleyd <subcommand> --help` for subcommand-specific options.\n")
}

// cmdHealthcheck hits /healthz on the local server and exits 0 on success,
// 1 on failure. Designed for use as a Docker HEALTHCHECK command so the
// container needs no curl or wget.
func cmdHealthcheck() int {
	addr := os.Getenv("PARLEY_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	// addr may be ":8080" or "0.0.0.0:8080"; always probe localhost.
	host := "localhost"
	if _, port, ok := strings.Cut(addr, ":"); ok {
		host = "localhost:" + port
	}
	resp, err := http.Get("http://" + host + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy:", resp.Status)
		return 1
	}
	return 0
}

// safeDB returns a log-safe representation of a DSN: credentials are stripped
// from Postgres URLs; SQLite paths and ":memory:" are returned as-is.
func safeDB(dsn string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return u.Scheme + "://<invalid>"
		}
		return u.Scheme + "://" + u.Host + u.Path
	}
	return dsn
}

// resolveDSN resolves the user-supplied PARLEY_DB value into a DSN ready for
// store.Open. PostgreSQL DSNs ("postgres://" or "postgresql://") and the
// special ":memory:" value are passed through as-is. Everything else is
// treated as a SQLite file path; the parent directory is created if needed.
func resolveDSN(env string) (string, error) {
	if env == ":memory:" ||
		strings.HasPrefix(env, "postgres://") ||
		strings.HasPrefix(env, "postgresql://") {
		return env, nil
	}
	path := env
	if path == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(dir, "parley", "parleyd.db")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, nil
}
