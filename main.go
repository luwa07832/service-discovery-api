// Command service-discovery-api serves the HTTP API described in README.md.
package main

import (
	"log"
	"os"

	"github.com/luwa07832/service-discovery-api/internal/api"
	"github.com/luwa07832/service-discovery-api/internal/store"
)

func main() {
	address := os.Getenv("ADDR")
	if address == "" {
		address = "127.0.0.1:8080"
	}

	// The registry is in-process only: nothing is read from or written to
	// disk, so no database path is needed.
	st, err := store.Open("")
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	if err := api.NewRouter(st).Run(address); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
