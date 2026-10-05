package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"
)

// A node directory contains raw Blake2-addressed trie nodes exported from a
// retained parent state. This command never opens the node's mutable database.
func runHistoricalCaptureCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("capture-historical-execution", flag.ContinueOnError)
	flags.SetOutput(stderr)
	engine := flags.String("engine", "", "absolute independently reviewed capture executable")
	engineHash := flags.String("engine-sha256", "", "exact capture executable sha256:DIGEST")
	input := flags.String("request", "", "absolute exact parent/child/body/runtime capture request")
	inputHash := flags.String("request-sha256", "", "exact request sha256:DIGEST")
	nodes := flags.String("nodes", "", "absolute read-only raw parent trie node directory")
	budget := flags.Duration("budget", 300*time.Second, "one total process/read budget,60s–15m")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || !bootstrapRootAbsolutePath(*engine) || !planSha256(*engineHash) || !bootstrapRootAbsolutePath(*input) || !planSha256(*inputHash) || !bootstrapRootAbsolutePath(*nodes) || *budget < time.Minute || *budget > 15*time.Minute {
		fmt.Fprintln(stderr, "capture-historical-execution requires exact --engine/--engine-sha256, --request/--request-sha256, --nodes and a60s–15m budget")
		return 2
	}
	report, err := runHistoricalCapture(ctx, historicalCaptureRequest{Engine: planFileReference{Path: *engine, Sha256: *engineHash}, Input: planFileReference{Path: *input, Sha256: *inputHash}, Nodes: *nodes, Budget: *budget}, historicalReplayHooks{})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(report); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
