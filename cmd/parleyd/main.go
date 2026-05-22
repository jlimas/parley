// Command parleyd is the parley broker server.
//
// It accepts events POSTed by clients and fans them out to subscribers over
// Server-Sent Events. For now only /healthz is wired up.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/yalochat/parley/internal/server"
)

func main() {
	addr := os.Getenv("PARLEY_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := server.New()
	log.Printf("parleyd listening on %s", addr)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		log.Fatalf("parleyd: %v", err)
	}
}
