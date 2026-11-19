package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/kayushkin/llm-bridge/machinestats"
	"github.com/kayushkin/llm-bridge/msg"
)

// The runner reads its machine's health — CPU, memory, drives, batteries,
// uptime — once a minute while it is connected, and sends each reading to the
// bridge, which keeps the history and draws the Machines page. In WSL the
// reading is of the Windows machine (package machinestats says how).

// sendMachineStatsWhileConnected sends a reading at once and then every
// machinestats.SampleInterval until ctx ends with the connection.
func (c *Client) sendMachineStatsWhileConnected(ctx context.Context) {
	ticker := time.NewTicker(machinestats.SampleInterval)
	defer ticker.Stop()
	for {
		c.sendMachineStats(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sendMachineStats takes one reading and queues it. A full queue means the
// connection is stalled; the reading is dropped rather than held, since the
// next one replaces it.
func (c *Client) sendMachineStats(ctx context.Context) {
	stats := c.machineStatsCollector.Collect(ctx)
	if ctx.Err() != nil {
		return
	}
	for _, collectionError := range stats.CollectionErrors {
		log.Printf("[machine-stats] %s", collectionError)
	}
	select {
	case c.outgoing <- &msg.RunnerMessage{Type: msg.RunnerMsgMachineStats, MachineStats: &msg.RunnerMachineStats{Stats: stats}}:
	default:
		log.Printf("[machine-stats] outgoing queue full; dropped a reading")
	}
}

// printMachineStats is `llm-bridge-runner machine-stats`: one reading of this
// machine, as the bridge would receive it.
func printMachineStats() {
	stats := machinestats.NewCollector().Collect(context.Background())
	encoded, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
}
