// Command aquaprofiled runs the continuous profiling HTTP service.
package main

import (
	"flag"
	"log"
	"net/http"

	"aquaprofile/internal/server"
	"aquaprofile/internal/store"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	st := store.New()
	log.Printf("aquaprofile profiling service listening on %s", *addr)
	if err := http.ListenAndServe(*addr, server.New(st)); err != nil {
		log.Fatal(err)
	}
}
