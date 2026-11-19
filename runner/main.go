// llm-bridge-runner registers a remote machine with an llm-bridge-server
// over an outbound WebSocket and accepts harness-spawn requests. Enables
// driving harness subprocesses on machines behind NAT without inbound SSH.
//
// Two subcommands:
//
//	llm-bridge-runner enroll --server URL --passphrase XXXX [--name LABEL]
//	    One-time exchange: trade a single-use passphrase for a durable
//	    per-machine runner token. Writes the token to ~/.config/llm-bridge-runner/config.json.
//
//	llm-bridge-runner [run]    (default)
//	    Long-lived service. Reads the saved config and dials the server.
//
// The default invocation requires that `enroll` has run successfully at
// least once, since the bearer token authenticates every connection.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "enroll":
			enrollCmd(os.Args[2:])
			return
		case "run":
			runService(os.Args[2:])
			return
		case "machine-stats":
			printMachineStats()
			return
		case "-version":
			fmt.Println(Version)
			return
		case "-h", "--help":
			usage()
			return
		}
	}
	runService(os.Args[1:])
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  llm-bridge-runner enroll --server URL --passphrase XXXX [--name LABEL]
      Trade a single-use passphrase for a durable runner token.

  llm-bridge-runner [run]
      Read the saved config and run the WS client (default).

  llm-bridge-runner machine-stats
      Print one reading of this machine's health, as the bridge receives it.

  llm-bridge-runner -version
      Print version.`)
}

func enrollCmd(args []string) {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	server := fs.String("server", os.Getenv("LLMBRIDGE_SERVER"), "llm-bridge-server base URL")
	endpoint := fs.String("endpoint", os.Getenv("LLMBRIDGE_ENDPOINT"), "original install-time bridge spec (URL or user@host); recorded for idempotent re-install")
	passphrase := fs.String("passphrase", os.Getenv("LLMBRIDGE_PASSPHRASE"), "single-use enrollment passphrase from `llm-bridge-server mint-enroll`")
	name := fs.String("name", os.Getenv("LLMBRIDGE_MACHINE_NAME"), "machine label (defaults to hostname)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *server == "" || *passphrase == "" {
		fmt.Fprintln(os.Stderr, "error: --server and --passphrase are required")
		fs.PrintDefaults()
		os.Exit(2)
	}
	cfg, err := Enroll(*server, *passphrase, *name, *endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "enrollment failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("enrolled as %s (machine_id=%s)\n", cfg.MachineName, cfg.MachineID)
	fmt.Printf("config saved to %s\n", configPath())
}

func runService(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	cfg, err := LoadRuntimeConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("[runner] starting version=%s machine=%s server=%s", Version, cfg.MachineName, cfg.ServerURL)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	NewClient(cfg).Run(ctx)
}
