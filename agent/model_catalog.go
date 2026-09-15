package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
)

const (
	ModelCatalogVersion = 1
	modelRAMReserve     = uint64(1024 * 1024 * 1024)
	maxModelDownload    = int64(16 << 30)
)

// HostCapabilities is a local, point-in-time resource profile. It contains no
// host name, network address, or stable machine identifier.
type HostCapabilities struct {
	OS                   string       `json:"os"`
	Architecture         string       `json:"architecture"`
	LogicalCPUs          int          `json:"logical_cpus"`
	CPUFeatures          []string     `json:"cpu_features,omitempty"`
	MemoryTotalBytes     uint64       `json:"memory_total_bytes"`
	MemoryAvailableBytes uint64       `json:"memory_available_bytes"`
	ModelDirFreeBytes    uint64       `json:"model_dir_free_bytes"`
	NvidiaGPUs           []GPUProfile `json:"nvidia_gpus,omitempty"`
	GPUProbe             string       `json:"gpu_probe"`
}

type GPUProfile struct {
	Name             string `json:"name"`
	MemoryTotalBytes uint64 `json:"memory_total_bytes"`
	MemoryFreeBytes  uint64 `json:"memory_free_bytes"`
}

// ModelArtifact is a maintainer-curated immutable model download record.
// MinimumMemoryBytes is an estimate for catalog filtering, never a guarantee.
type ModelArtifact struct {
	ID                     string   `json:"id"`
	Name                   string   `json:"name"`
	Repository             string   `json:"repository"`
	Revision               string   `json:"revision"`
	Filename               string   `json:"filename"`
	SizeBytes              int64    `json:"size_bytes"`
	SHA256                 string   `json:"sha256"`
	License                string   `json:"license"`
	Quantization           string   `json:"quantization"`
	SupportedOS            []string `json:"supported_os"`
	SupportedArchitectures []string `json:"supported_architectures"`
	MinimumLogicalCPUs     int      `json:"minimum_logical_cpus"`
	MinimumMemoryBytes     uint64   `json:"minimum_memory_bytes"`
	MinimumDiskBytes       uint64   `json:"minimum_disk_bytes"`
	RequiredCPUFeatures    []string `json:"required_cpu_features,omitempty"`
	RequiresNvidiaGPU      bool     `json:"requires_nvidia_gpu,omitempty"`
	MinimumGPUFreeBytes    uint64   `json:"minimum_gpu_free_bytes,omitempty"`
	EvaluationStatus       string   `json:"evaluation_status"`
	EvaluationNote         string   `json:"evaluation_note"`
	DeploymentEligible     bool     `json:"deployment_eligible"`
}

type ModelAssessment struct {
	ModelID            string   `json:"model_id"`
	ResourceCompatible bool     `json:"resource_compatible"`
	DeploymentEligible bool     `json:"deployment_eligible"`
	Reasons            []string `json:"reasons"`
}

type ModelRecommendation struct {
	Model      ModelArtifact   `json:"model"`
	Assessment ModelAssessment `json:"assessment"`
}

var catalogRevisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var modelRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ModelCatalog contains only artifacts that have fixed revisions and hashes.
// The current models are evaluation-only because neither passed the agent's
// frozen planner quality/safety gate.
func ModelCatalog() []ModelArtifact {
	return []ModelArtifact{
		{
			ID: "qwen35-08b-q4_0-eval", Name: "Qwen3.5-0.8B Q4_0 (evaluation only)",
			Repository: "ggml-org/Qwen3.5-0.8B-GGUF", Revision: "8fea620810c4afa23dd6443f999a48574c1611a3",
			Filename: "Qwen3.5-0.8B-Q4_0.gguf", SizeBytes: 563036064,
			SHA256:  "57d1997790d1744fba5b40a7317df71ea5e2acee28c47e78f0cce39c0703f8cf",
			License: "Apache-2.0", Quantization: "Q4_0", SupportedOS: []string{"linux"},
			SupportedArchitectures: []string{"amd64", "arm64"},
			MinimumLogicalCPUs:     2,
			MinimumMemoryBytes:     2200 * 1024 * 1024, MinimumDiskBytes: 768 * 1024 * 1024,
			EvaluationStatus: "rejected", EvaluationNote: "Scored 33/51 on the expanded planner suite, with 3 unsafe proposals; do not deploy.",
			DeploymentEligible: false,
		},
		{
			ID: "qwen3-1_7b-q4_k_m-eval", Name: "Qwen3-1.7B Q4_K_M (evaluation only)",
			Repository: "ggml-org/Qwen3-1.7B-GGUF", Revision: "daeb8e2d528a760970442092f6bf1e55c3b659eb",
			Filename: "Qwen3-1.7B-Q4_K_M.gguf", SizeBytes: 1282439264,
			SHA256:  "d2387ca2dbfee2ffabce7120d3770dadca0b293052bc2f0e138fdc940d9bc7b5",
			License: "Apache-2.0", Quantization: "Q4_K_M", SupportedOS: []string{"linux"},
			SupportedArchitectures: []string{"amd64", "arm64"},
			MinimumLogicalCPUs:     2,
			MinimumMemoryBytes:     3000 * 1024 * 1024, MinimumDiskBytes: 1536 * 1024 * 1024,
			EvaluationStatus: "rejected", EvaluationNote: "Scored 36/51 on the expanded planner suite and missed 15 decisions; do not deploy.",
			DeploymentEligible: false,
		},
	}
}

func ValidateModelCatalog(catalog []ModelArtifact) error {
	if len(catalog) == 0 {
		return errors.New("model catalog is empty")
	}
	ids := make(map[string]bool, len(catalog))
	for _, artifact := range catalog {
		if artifact.ID == "" || ids[artifact.ID] || artifact.Name == "" {
			return errors.New("model catalog contains a missing or duplicate id")
		}
		ids[artifact.ID] = true
		if !catalogRevisionPattern.MatchString(artifact.Revision) || !sha256Pattern.MatchString(artifact.SHA256) {
			return fmt.Errorf("model %q must use a full immutable revision and SHA-256", artifact.ID)
		}
		if artifact.SizeBytes <= 0 || artifact.SizeBytes > maxModelDownload || artifact.MinimumLogicalCPUs <= 0 || artifact.MinimumMemoryBytes == 0 || artifact.MinimumDiskBytes < uint64(artifact.SizeBytes) {
			return fmt.Errorf("model %q has invalid resource limits", artifact.ID)
		}
		if artifact.License == "" || artifact.Filename == "" || filepath.Base(artifact.Filename) != artifact.Filename || !modelRepositoryPattern.MatchString(artifact.Repository) {
			return fmt.Errorf("model %q has invalid artifact metadata", artifact.ID)
		}
	}
	return nil
}

func AssessModels(profile HostCapabilities, catalog []ModelArtifact) ([]ModelAssessment, error) {
	if err := ValidateModelCatalog(catalog); err != nil {
		return nil, err
	}
	usableRAM := profile.MemoryAvailableBytes
	if profile.MemoryTotalBytes <= modelRAMReserve {
		usableRAM = 0
	} else if ceiling := profile.MemoryTotalBytes - modelRAMReserve; usableRAM > ceiling {
		usableRAM = ceiling
	}
	assessments := make([]ModelAssessment, 0, len(catalog))
	for _, artifact := range catalog {
		result := ModelAssessment{ModelID: artifact.ID}
		osMatch := containsString(artifact.SupportedOS, profile.OS)
		archMatch := containsString(artifact.SupportedArchitectures, profile.Architecture)
		cpuCountMatch := profile.LogicalCPUs >= artifact.MinimumLogicalCPUs
		ramMatch := usableRAM >= artifact.MinimumMemoryBytes
		diskMatch := profile.ModelDirFreeBytes >= artifact.MinimumDiskBytes
		cpuMatch := hasAllFeatures(profile.CPUFeatures, artifact.RequiredCPUFeatures)
		gpuMatch := !artifact.RequiresNvidiaGPU
		if artifact.RequiresNvidiaGPU {
			for _, gpu := range profile.NvidiaGPUs {
				if gpu.MemoryFreeBytes >= artifact.MinimumGPUFreeBytes {
					gpuMatch = true
					break
				}
			}
		}
		if !osMatch {
			result.Reasons = append(result.Reasons, "host OS is not supported by this artifact")
		}
		if !archMatch {
			result.Reasons = append(result.Reasons, "host architecture is not supported by this artifact")
		}
		if !cpuCountMatch {
			result.Reasons = append(result.Reasons, "host has fewer logical CPUs than the catalog minimum")
		}
		if !ramMatch {
			result.Reasons = append(result.Reasons, "available RAM after a 1 GiB host reserve is below the catalog estimate")
		}
		if !diskMatch {
			result.Reasons = append(result.Reasons, "model directory does not have enough free disk space")
		}
		if !cpuMatch {
			result.Reasons = append(result.Reasons, "host CPU does not expose all required instruction features")
		}
		if !gpuMatch {
			result.Reasons = append(result.Reasons, "no NVIDIA GPU has enough currently free VRAM")
		}
		result.ResourceCompatible = osMatch && archMatch && cpuCountMatch && ramMatch && diskMatch && cpuMatch && gpuMatch
		if artifact.EvaluationStatus != "passed" || !artifact.DeploymentEligible {
			result.Reasons = append(result.Reasons, "model has not passed the release evaluation gate; evaluation use only")
		}
		result.DeploymentEligible = result.ResourceCompatible && artifact.EvaluationStatus == "passed" && artifact.DeploymentEligible
		if result.ResourceCompatible && len(result.Reasons) == 0 {
			result.Reasons = []string{"resource estimate fits; run the planner evaluation on this host before selecting it"}
		}
		assessments = append(assessments, result)
	}
	return assessments, nil
}

func FindModel(catalog []ModelArtifact, id string) (ModelArtifact, bool) {
	for _, artifact := range catalog {
		if artifact.ID == id {
			return artifact, true
		}
	}
	return ModelArtifact{}, false
}

func RecommendModels(profile HostCapabilities, catalog []ModelArtifact) ([]ModelRecommendation, error) {
	assessments, err := AssessModels(profile, catalog)
	if err != nil {
		return nil, err
	}
	recommendations := make([]ModelRecommendation, 0, len(catalog))
	for i, model := range catalog {
		recommendations = append(recommendations, ModelRecommendation{Model: model, Assessment: assessments[i]})
	}
	return recommendations, nil
}

// DownloadModel downloads only a catalogued Hugging Face artifact pinned to a
// full commit and verifies its expected length and SHA-256 before publishing it
// in the chosen directory. Unqualified models require evaluationOnly=true.
func DownloadModel(ctx context.Context, artifact ModelArtifact, profile HostCapabilities, directory string, evaluationOnly bool) (string, error) {
	if err := ValidateModelCatalog([]ModelArtifact{artifact}); err != nil {
		return "", err
	}
	if artifact.EvaluationStatus != "passed" || !artifact.DeploymentEligible {
		if !evaluationOnly {
			return "", errors.New("catalog model is not release-qualified; pass evaluation-only intent to download it for testing")
		}
	}
	if !containsString(artifact.SupportedOS, profile.OS) || !containsString(artifact.SupportedArchitectures, profile.Architecture) {
		return "", errors.New("model artifact does not support this host platform")
	}
	assessments, err := AssessModels(profile, []ModelArtifact{artifact})
	if err != nil {
		return "", err
	}
	if !assessments[0].ResourceCompatible {
		return "", errors.New("model does not fit this host's current memory and disk profile")
	}
	if _, err := verifyRealDirectory(directory); err != nil {
		return "", fmt.Errorf("inspect model directory: %w", err)
	}
	free, err := diskFree(directory)
	if err != nil {
		return "", fmt.Errorf("inspect model directory free space: %w", err)
	}
	if free < artifact.MinimumDiskBytes {
		return "", errors.New("model directory does not have enough free space for the catalog requirement")
	}
	baseURL := "https://huggingface.co/" + artifact.Repository + "/resolve/" + artifact.Revision + "/" + url.PathEscape(artifact.Filename) + "?download=true"
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "huggingface.co" {
		return "", errors.New("catalog artifact URL is not an approved HTTPS Hugging Face URL")
	}
	client := &http.Client{
		Timeout: 0,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 8 || request.URL.Scheme != "https" || request.URL.User != nil {
				return errors.New("model download redirect was refused")
			}
			return nil
		},
	}
	return downloadVerified(ctx, client, parsed.String(), directory, artifact.Filename, artifact.SizeBytes, artifact.SHA256)
}

func downloadVerified(ctx context.Context, client *http.Client, sourceURL, directory, filename string, expectedSize int64, expectedSHA string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return "", fmt.Errorf("create model download request: %w", err)
	}
	request.Header.Set("Accept-Encoding", "identity")
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("download model artifact: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("model source returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength >= 0 && response.ContentLength != expectedSize {
		return "", errors.New("model source content length does not match the catalog")
	}
	destination := filepath.Join(directory, filename)
	hasher := sha256.New()
	var written int64
	write := func(temp *os.File) error {
		n, err := io.Copy(io.MultiWriter(temp, hasher), io.LimitReader(response.Body, expectedSize+1))
		written = n
		if err != nil {
			return fmt.Errorf("write temporary model file: %w", err)
		}
		return nil
	}
	publish := func(tempPath string) error {
		if written != expectedSize || hex.EncodeToString(hasher.Sum(nil)) != expectedSHA {
			return errors.New("model artifact size or SHA-256 verification failed")
		}
		if err := os.Link(tempPath, destination); err != nil {
			return fmt.Errorf("publish verified model without overwriting: %w", err)
		}
		if err := os.Remove(tempPath); err != nil {
			os.Remove(destination)
			return fmt.Errorf("remove verified temporary model file: %w", err)
		}
		return nil
	}
	if err := writeFileAtomic0600(directory, ".nostrhost-model-*.part", write, publish); err != nil {
		return "", err
	}
	return destination, nil
}

// InferenceRuntimeArtifact is a maintainer-curated, immutable llama.cpp CPU
// build used to serve a catalogued GGUF model over its OpenAI-compatible
// server. Pinned to an exact release tag and verified by size and SHA-256,
// the same way ModelArtifact downloads are.
type InferenceRuntimeArtifact struct {
	ID                     string   `json:"id"`
	Tag                    string   `json:"tag"`
	AssetName              string   `json:"asset_name"`
	ServerBinary           string   `json:"server_binary"`
	SizeBytes              int64    `json:"size_bytes"`
	SHA256                 string   `json:"sha256"`
	SupportedOS            []string `json:"supported_os"`
	SupportedArchitectures []string `json:"supported_architectures"`
}

var runtimeTagPattern = regexp.MustCompile(`^b[0-9]{1,7}$`)

// InferenceRuntimeCatalog returns the single pinned llama.cpp CPU build
// validated on the nostrhost-agent test VM (see docs/model-selection.md).
func InferenceRuntimeCatalog() []InferenceRuntimeArtifact {
	return []InferenceRuntimeArtifact{
		{
			ID: "llama-cpp-b10950-cpu", Tag: "b10950",
			AssetName: "llama-b10950-bin-ubuntu-x64.tar.gz", ServerBinary: "llama-server",
			SizeBytes:   16822383,
			SHA256:      "db40ef24d13ab23d6fd486eae219f634edc5ed529c2c217e71b4ef0d4572417c",
			SupportedOS: []string{"linux"}, SupportedArchitectures: []string{"amd64"},
		},
	}
}

func ValidateRuntimeCatalog(catalog []InferenceRuntimeArtifact) error {
	if len(catalog) == 0 {
		return errors.New("inference runtime catalog is empty")
	}
	ids := make(map[string]bool, len(catalog))
	for _, artifact := range catalog {
		if artifact.ID == "" || ids[artifact.ID] {
			return errors.New("inference runtime catalog contains a missing or duplicate id")
		}
		ids[artifact.ID] = true
		if !runtimeTagPattern.MatchString(artifact.Tag) || !sha256Pattern.MatchString(artifact.SHA256) {
			return fmt.Errorf("runtime %q must use a pinned release tag and SHA-256", artifact.ID)
		}
		if artifact.SizeBytes <= 0 || artifact.SizeBytes > maxModelDownload || artifact.AssetName == "" ||
			filepath.Base(artifact.AssetName) != artifact.AssetName || artifact.ServerBinary == "" {
			return fmt.Errorf("runtime %q has invalid artifact metadata", artifact.ID)
		}
	}
	return nil
}

func FindRuntime(catalog []InferenceRuntimeArtifact, id string) (InferenceRuntimeArtifact, bool) {
	for _, artifact := range catalog {
		if artifact.ID == id {
			return artifact, true
		}
	}
	return InferenceRuntimeArtifact{}, false
}

// DownloadRuntime fetches and verifies a pinned llama.cpp release tarball from
// its GitHub release, but does not unpack it — the caller (nostrhost-agent-model
// runtime download) does that after this returns, so the verified archive is
// never extracted from an unverified byte stream.
func DownloadRuntime(ctx context.Context, artifact InferenceRuntimeArtifact, profile HostCapabilities, directory string) (string, error) {
	if err := ValidateRuntimeCatalog([]InferenceRuntimeArtifact{artifact}); err != nil {
		return "", err
	}
	if !containsString(artifact.SupportedOS, profile.OS) || !containsString(artifact.SupportedArchitectures, profile.Architecture) {
		return "", errors.New("inference runtime artifact does not support this host platform")
	}
	if _, err := verifyRealDirectory(directory); err != nil {
		return "", fmt.Errorf("inspect runtime directory: %w", err)
	}
	free, err := diskFree(directory)
	if err != nil {
		return "", fmt.Errorf("inspect runtime directory free space: %w", err)
	}
	if free < uint64(artifact.SizeBytes)*2 {
		return "", errors.New("runtime directory does not have enough free space to download and unpack")
	}
	sourceURL := fmt.Sprintf("https://github.com/ggml-org/llama.cpp/releases/download/%s/%s", artifact.Tag, artifact.AssetName)
	parsed, err := url.Parse(sourceURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" {
		return "", errors.New("catalog runtime URL is not an approved HTTPS GitHub URL")
	}
	client := &http.Client{
		Timeout: 0,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 8 || request.URL.Scheme != "https" || request.URL.User != nil {
				return errors.New("runtime download redirect was refused")
			}
			return nil
		},
	}
	return downloadVerified(ctx, client, parsed.String(), directory, artifact.AssetName, artifact.SizeBytes, artifact.SHA256)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func hasAllFeatures(have, required []string) bool {
	for _, feature := range required {
		if !containsString(have, feature) {
			return false
		}
	}
	return true
}
