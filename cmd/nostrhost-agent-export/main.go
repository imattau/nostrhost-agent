package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/imattau/nostrhost-agent/agent"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "nostrhost-agent-export: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("nostrhost-agent-export", flag.ContinueOnError)
	journal := flags.String("journal", "", "path to the private local audit journal")
	list := flags.Bool("list", false, "list export-eligible cycles instead of preparing one")
	cycleID := flags.String("cycle-id", "", "explicit finalized cycle ID to prepare for review")
	output := flags.String("output", "", "new file path for the local review candidate")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *journal == "" {
		return fmt.Errorf("usage: nostrhost-agent-export --journal PATH [--list | --cycle-id ID --output NEW_PATH]")
	}
	if *list {
		if *cycleID != "" || *output != "" {
			return fmt.Errorf("--list cannot be combined with --cycle-id or --output")
		}
		summaries, err := agent.ListExportableCycles(*journal)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summaries)
	}
	if *cycleID == "" || *output == "" {
		return fmt.Errorf("usage: nostrhost-agent-export --journal PATH --cycle-id ID --output NEW_PATH")
	}
	trace, err := agent.ReadContributionCycle(*journal, *cycleID)
	if err != nil {
		return err
	}
	candidate, err := agent.BuildContributionCandidate(trace)
	if err != nil {
		return err
	}
	if err := agent.WriteContributionCandidate(*output, candidate); err != nil {
		return err
	}
	fmt.Printf("Wrote local review candidate to %s (%d redactions). Review before sharing; no data was uploaded.\n", *output, candidate.RedactionsApplied)
	return nil
}
