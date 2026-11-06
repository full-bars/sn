package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync/atomic"
)

// identityGateServer counts registration requests without recording credentials.
func identityGateServer(addrFile string) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	var registrations atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/network/register-client-v1", func(w http.ResponseWriter, r *http.Request) {
		registrations.Add(1)
		http.Error(w, "fixture rejects registrations", http.StatusForbidden)
	})
	mux.HandleFunc("/__calls", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			registrations.Store(0)
		}
		fmt.Fprintln(w, registrations.Load())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fixture rejects requests", http.StatusForbidden)
	})
	tmpFile := addrFile + ".tmp"
	if err := os.WriteFile(tmpFile, []byte("http://"+listener.Addr().String()), 0600); err != nil {
		log.Fatal(err)
	}
	if err := os.Rename(tmpFile, addrFile); err != nil {
		log.Fatal(err)
	}
	log.Fatal(http.Serve(listener, mux))
}
