// Package main implements a minimal fake provider for CI lifecycle testing.
// It listens on the Unix domain socket at ~/.urnetwork/provider.sock and
// responds to the same JSON-over-connection protocol as the real provider.
//
// The fake provider handles exactly two command types:
//
//   - {"cmd":"shutdown"} — write {"ok":true} then exit(0)
//   - anything else      — write {"ok":true}
//
// This is enough to exercise urnet-tools start/stop/restart/logs on Windows
// CI without a real provider binary.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

var (
	ln       net.Listener
	sockPath string
)

// controlRequest mirrors the wire format from the real provider.
type controlRequest struct {
	Cmd   string `json:"cmd"`
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

// controlResponse is the fake provider's reply.
type controlResponse struct {
	OK           bool   `json:"ok"`
	Value        string `json:"value,omitempty"`
	BuildVersion string `json:"build_version,omitempty"`
	Error        string `json:"error,omitempty"`
}

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake-provider: cannot determine home directory: %v\n", err)
		os.Exit(1)
	}

	sockDir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "fake-provider: cannot create state dir: %v\n", err)
		os.Exit(1)
	}

	sockPath = filepath.Join(sockDir, "provider.sock")

	// Remove a stale socket from a previous run.
	_ = os.Remove(sockPath)

	ln, err = net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake-provider: cannot bind %s: %v\n", sockPath, err)
		os.Exit(1)
	}
	defer ln.Close()
	_ = os.Chmod(sockPath, 0o600)

	fmt.Fprintf(os.Stderr, "fake-provider: listening on %s (pid %d)\n", sockPath, os.Getpid())

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			fmt.Fprintf(os.Stderr, "fake-provider: accept: %v\n", err)
			continue
		}
		go handleConn(conn)
	}
}

func handleConn(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}

	var req controlRequest
	if err := json.Unmarshal(line, &req); err != nil {
		resp := controlResponse{OK: false, Error: fmt.Sprintf("bad request: %v", err)}
		json.NewEncoder(conn).Encode(resp)
		return
	}

	resp := controlResponse{OK: true}

	switch req.Cmd {
	case "shutdown":
		// Acknowledge then signal the main loop to exit.
		json.NewEncoder(conn).Encode(resp)
		fmt.Fprintf(os.Stderr, "fake-provider: shutdown requested, exiting\n")
		ln.Close()
		_ = os.Remove(sockPath)
		go func() {
			time.Sleep(50 * time.Millisecond)
			os.Exit(0)
		}()
		return
	case "version":
		resp.BuildVersion = "fake-provider-0.0.1"
	default:
		resp = controlResponse{OK: false, Error: "hotswap not supported"}
	}

	json.NewEncoder(conn).Encode(resp)
}
