package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/bfulton/sesh/internal/server"
)

// cmdServe runs a storage server that only ever sees ciphertext. Point
// clients at it with sesh init -store https://your-host. Put it behind TLS.
func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "address to listen on")
	dir := fs.String("dir", filepath.Join(seshHome(), "server"), "where to keep objects")
	fs.Parse(args)
	srv, err := server.New(*dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "sesh: serving %s on %s\n", *dir, *addr)
	hs := &http.Server{
		Addr:              *addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
	}
	return hs.ListenAndServe()
}
