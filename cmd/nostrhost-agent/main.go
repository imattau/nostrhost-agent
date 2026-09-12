package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/imattau/nostrhost-agent/agent"
)

func main() {
	if err := run(); err != nil {
		log.Printf("nostrhost-agent stopped: %v", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/nostrhost-agent/config.json", "path to the private runtime configuration")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	config, err := agent.LoadRuntimeConfig(*configPath)
	if err != nil {
		return err
	}
	runtime, err := agent.NewResidentRuntime(config)
	if err != nil {
		return err
	}
	defer runtime.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runtime.Run(ctx)
}
