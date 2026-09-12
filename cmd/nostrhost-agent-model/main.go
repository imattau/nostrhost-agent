package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/imattau/nostrhost-agent/agent"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "nostrhost-agent-model: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: nostrhost-agent-model profile|recommend|download [flags]")
	}
	command, args := args[0], args[1:]
	flags := flag.NewFlagSet("nostrhost-agent-model "+command, flag.ContinueOnError)
	modelsDirectory := flags.String("models-dir", "", "existing model directory owned by the account that runs the agent")
	modelID := flags.String("model-id", "", "catalog model id to download")
	evaluationOnly := flags.Bool("evaluation-only", false, "explicitly allow downloading a model that has not passed release evaluation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if command != "profile" && command != "recommend" && command != "download" {
		return errors.New("command must be profile, recommend, or download")
	}
	if *modelsDirectory == "" {
		return errors.New("--models-dir is required and must be writable by the account that will run the agent")
	}
	if command == "download" {
		if *modelID == "" {
			return errors.New("--model-id is required for download")
		}
	}
	profile, err := agent.ProbeHostCapabilities(*modelsDirectory)
	if err != nil {
		return err
	}
	switch command {
	case "profile":
		return printJSON(profile)
	case "recommend":
		recommendations, err := agent.RecommendModels(profile, agent.ModelCatalog())
		if err != nil {
			return err
		}
		return printJSON(struct {
			CatalogVersion int                         `json:"catalog_version"`
			Profile        agent.HostCapabilities      `json:"profile"`
			Models         []agent.ModelRecommendation `json:"models"`
		}{agent.ModelCatalogVersion, profile, recommendations})
	case "download":
		artifact, ok := agent.FindModel(agent.ModelCatalog(), *modelID)
		if !ok {
			return fmt.Errorf("unknown model id %q", *modelID)
		}
		assessments, err := agent.AssessModels(profile, []agent.ModelArtifact{artifact})
		if err != nil {
			return err
		}
		if !assessments[0].ResourceCompatible {
			return fmt.Errorf("model is not recommended for current host profile: %v", assessments[0].Reasons)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		path, err := agent.DownloadModel(ctx, artifact, profile, filepath.Clean(*modelsDirectory), *evaluationOnly)
		if err != nil {
			return err
		}
		return printJSON(struct {
			ModelID string `json:"model_id"`
			Path    string `json:"path"`
			SHA256  string `json:"sha256"`
			Message string `json:"message"`
		}{artifact.ID, path, artifact.SHA256, "download verified; active agent configuration was not changed"})
	default:
		return errors.New("unreachable model command")
	}
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
