// Command parleyd is the parley broker server.
//
// It accepts events POSTed by clients and fans them out to subscribers over
// Server-Sent Events. State is persisted to a SQLite database; restarting
// the broker preserves the board.
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/jlimas/parley/internal/server"
	"github.com/jlimas/parley/internal/store"
)

func main() {
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
	log.Printf("parleyd: db=%s replayed=%d", dsn, len(initial))

	srv := server.New(db, initial)
	log.Printf("parleyd listening on %s", addr)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		log.Fatalf("parleyd: %v", err)
	}
}

// resolveDSN expands the user-supplied PARLEY_DB value into a SQLite DSN and
// ensures the parent directory exists for file-backed paths. The literal
// ":memory:" is passed through untouched for ephemeral runs.
func resolveDSN(env string) (string, error) {
	if env == ":memory:" {
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
