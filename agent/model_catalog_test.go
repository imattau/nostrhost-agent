package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testModelProfile() HostCapabilities {
	return HostCapabilities{
		OS: "linux", Architecture: "amd64", LogicalCPUs: 4,
		MemoryTotalBytes:     4 * 1024 * 1024 * 1024,
		MemoryAvailableBytes: 3 * 1024 * 1024 * 1024,
		ModelDirFreeBytes:    4 * 1024 * 1024 * 1024,
	}
}

func TestModelCatalogIsPinnedAndExplicitlyUnqualified(t *testing.T) {
	catalog := ModelCatalog()
	if err := ValidateModelCatalog(catalog); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range catalog {
		if artifact.DeploymentEligible || artifact.EvaluationStatus != "rejected" {
			t.Errorf("unqualified model became deployment eligible: %#v", artifact)
		}
	}
}

func TestAssessModelsUsesAvailableHostResourcesAndEvaluationGate(t *testing.T) {
	catalog := ModelCatalog()
	profile := testModelProfile()
	results, err := AssessModels(profile, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].ResourceCompatible || !results[1].ResourceCompatible {
		t.Fatalf("4 GiB test host should pass both approximate resource filters: %#v", results)
	}
	for _, result := range results {
		if result.DeploymentEligible {
			t.Fatalf("resource fit incorrectly bypassed model evaluation gate: %#v", result)
		}
	}
	profile.MemoryAvailableBytes = 1800 * 1024 * 1024
	results, err = AssessModels(profile, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].ResourceCompatible || results[1].ResourceCompatible {
		t.Fatalf("low-memory host reported a model fit: %#v", results)
	}
}

func TestAssessModelsHonorsCPUFeatureAndGPURequirements(t *testing.T) {
	artifact := ModelCatalog()[0]
	artifact.ID = "hardware-specific-test"
	artifact.RequiredCPUFeatures = []string{"avx2"}
	artifact.RequiresNvidiaGPU = true
	artifact.MinimumGPUFreeBytes = 4 * 1024 * 1024 * 1024
	profile := testModelProfile()
	results, err := AssessModels(profile, []ModelArtifact{artifact})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].ResourceCompatible {
		t.Fatal("host without required CPU feature and GPU passed compatibility filter")
	}
	profile.CPUFeatures = []string{"avx2"}
	profile.NvidiaGPUs = []GPUProfile{{Name: "test gpu", MemoryFreeBytes: 6 * 1024 * 1024 * 1024}}
	results, err = AssessModels(profile, []ModelArtifact{artifact})
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].ResourceCompatible {
		t.Fatalf("host meeting CPU and GPU requirements was rejected: %#v", results[0])
	}
}

func TestDownloadModelRequiresExplicitEvaluationIntentAndCompatibleResources(t *testing.T) {
	artifact := ModelCatalog()[0]
	directory := t.TempDir()
	profile := testModelProfile()
	if _, err := DownloadModel(context.Background(), artifact, profile, directory, false); err == nil || !strings.Contains(err.Error(), "evaluation-only") {
		t.Fatalf("unqualified model downloaded without explicit evaluation intent: %v", err)
	}
	profile.MemoryAvailableBytes = 1200 * 1024 * 1024
	if _, err := DownloadModel(context.Background(), artifact, profile, directory, true); err == nil || !strings.Contains(err.Error(), "does not fit") {
		t.Fatalf("model downloaded despite an insufficient resource profile: %v", err)
	}
}

func TestDownloadVerifiedPublishesOnlyMatchingArtifact(t *testing.T) {
	body := []byte("small test model")
	digest := sha256.Sum256(body)
	hash := hex.EncodeToString(digest[:])
	client := &http.Client{Transport: modelTestTransport{body: body}}
	directory := t.TempDir()
	path, err := downloadVerified(context.Background(), client, "https://models.test/model", directory, "model.gguf", int64(len(body)), hash)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(body) {
		t.Fatalf("verified artifact contents = %q, err=%v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("downloaded artifact mode = %o, want 600", info.Mode().Perm())
	}
	if _, err := downloadVerified(context.Background(), client, "https://models.test/model", directory, "model.gguf", int64(len(body)), hash); err == nil {
		t.Fatal("download overwrote an existing model file")
	}
	badDir := t.TempDir()
	if _, err := downloadVerified(context.Background(), client, "https://models.test/model", badDir, "bad.gguf", int64(len(body)), strings.Repeat("0", 64)); err == nil {
		t.Fatal("artifact with wrong digest was accepted")
	}
	entries, err := os.ReadDir(badDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed download left files behind: %#v", entries)
	}
}

type modelTestTransport struct{ body []byte }

func (transport modelTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header),
		Body:          io.NopCloser(bytes.NewReader(transport.body)),
		ContentLength: int64(len(transport.body)), Request: request,
	}, nil
}

func TestDownloadModelRejectsExistingDirectorySymlinkAndInvalidCatalogHost(t *testing.T) {
	artifact := ModelCatalog()[0]
	profile := testModelProfile()
	root := t.TempDir()
	actual := filepath.Join(root, "actual")
	if err := os.Mkdir(actual, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	if _, err := DownloadModel(context.Background(), artifact, profile, link, true); err == nil {
		t.Fatal("symlink model directory accepted")
	}
	artifact.Repository = "../evil/model"
	if err := ValidateModelCatalog([]ModelArtifact{artifact}); err == nil {
		t.Fatal("untrusted repository path accepted")
	}
}
