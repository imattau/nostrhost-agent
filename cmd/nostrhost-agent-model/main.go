package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
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
		return errors.New("usage: nostrhost-agent-model profile|recommend|download|runtime [flags]")
	}
	command, args := args[0], args[1:]
	if command == "runtime" {
		return runRuntime(args)
	}
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
		return errors.New("command must be profile, recommend, download, or runtime")
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

func runRuntime(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: nostrhost-agent-model runtime status|download [flags]")
	}
	action, args := args[0], args[1:]
	flags := flag.NewFlagSet("nostrhost-agent-model runtime "+action, flag.ContinueOnError)
	runtimeDirectory := flags.String("runtime-dir", "", "existing directory owned by the account that runs the agent")
	runtimeID := flags.String("runtime-id", agent.InferenceRuntimeCatalog()[0].ID, "catalog inference runtime id")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if action != "status" && action != "download" {
		return errors.New("runtime action must be status or download")
	}
	if *runtimeDirectory == "" {
		return errors.New("--runtime-dir is required and must be writable by the account that will run the agent")
	}
	artifact, ok := agent.FindRuntime(agent.InferenceRuntimeCatalog(), *runtimeID)
	if !ok {
		return fmt.Errorf("unknown runtime id %q", *runtimeID)
	}
	serverPath := filepath.Join(*runtimeDirectory, artifact.ID, artifact.ServerBinary)
	if action == "status" {
		_, err := os.Stat(serverPath)
		return printJSON(struct {
			RuntimeID  string `json:"runtime_id"`
			Installed  bool   `json:"installed"`
			ServerPath string `json:"server_path,omitempty"`
		}{artifact.ID, err == nil, serverPathIfInstalled(serverPath, err)})
	}
	profile, err := agent.ProbeHostCapabilities(*runtimeDirectory)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	archivePath, err := agent.DownloadRuntime(ctx, artifact, profile, filepath.Clean(*runtimeDirectory))
	if err != nil {
		return err
	}
	defer os.Remove(archivePath)
	destination := filepath.Join(*runtimeDirectory, artifact.ID)
	if err := extractRuntimeArchive(archivePath, destination, artifact.ServerBinary); err != nil {
		return fmt.Errorf("unpack verified runtime archive: %w", err)
	}
	return printJSON(struct {
		RuntimeID  string `json:"runtime_id"`
		ServerPath string `json:"server_path"`
		SHA256     string `json:"sha256"`
		Message    string `json:"message"`
	}{artifact.ID, filepath.Join(destination, artifact.ServerBinary), artifact.SHA256, "download verified and unpacked; no service was started"})
}

func serverPathIfInstalled(path string, statErr error) string {
	if statErr != nil {
		return ""
	}
	return path
}

// extractRuntimeArchive unpacks an already hash-verified tar.gz into
// destination, flattening the archive's single top-level directory. Upstream
// llama.cpp archives use relative symlinks for shared-library sonames. We
// materialize those as hard links after extracting their regular-file targets,
// preserving a symlink-free runtime without dropping files the loader needs.
// Path traversal and links whose targets aren't extracted regular files remain
// forbidden.
func extractRuntimeArchive(archivePath, destination, requiredBinary string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	sawBinary := false
	type pendingLink struct {
		name   string
		target string
	}
	var links []pendingLink
	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeLink {
			continue
		}
		relative := header.Name
		if slash := strings.IndexByte(relative, '/'); slash >= 0 {
			relative = relative[slash+1:]
		}
		if relative == "" || strings.Contains(relative, "..") || filepath.IsAbs(relative) {
			continue
		}
		target := filepath.Join(destination, filepath.Clean(relative))
		if !strings.HasPrefix(target, filepath.Clean(destination)+string(os.PathSeparator)) {
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			// The runtime is public (an unmodified upstream release build),
			// so extracted files just need to be readable/executable by the
			// unprivileged service account that will actually run them.
			mode := os.FileMode(header.Mode) & 0o777
			if mode == 0 {
				mode = 0o644
			}
			mode |= 0o044
			if mode&0o100 != 0 {
				mode |= 0o011
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, reader); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			if relative == requiredBinary {
				sawBinary = true
			}
		case tar.TypeSymlink:
			// Resolve the archive link lexically before flattening the common
			// top-level directory. Absolute paths and any path escaping that
			// directory are discarded.
			if path.IsAbs(header.Linkname) {
				continue
			}
			targetName := path.Clean(path.Join(path.Dir(header.Name), header.Linkname))
			if targetName == "." || targetName == ".." || strings.HasPrefix(targetName, "../") {
				continue
			}
			targetRelative := targetName
			if slash := strings.IndexByte(targetRelative, '/'); slash >= 0 {
				targetRelative = targetRelative[slash+1:]
			}
			if targetRelative == "" || strings.Contains(targetRelative, "..") || filepath.IsAbs(targetRelative) {
				continue
			}
			links = append(links, pendingLink{name: relative, target: filepath.Clean(targetRelative)})
		}
	}
	for _, link := range links {
		target := filepath.Join(destination, link.target)
		info, err := os.Lstat(target)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("runtime link %q has no extracted regular-file target %q", link.name, link.target)
		}
		name := filepath.Join(destination, link.name)
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return err
		}
		if err := os.Link(target, name); err != nil {
			return fmt.Errorf("materialize runtime link %q: %w", link.name, err)
		}
	}
	if !sawBinary {
		return fmt.Errorf("archive did not contain the required %q binary", requiredBinary)
	}
	return nil
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
