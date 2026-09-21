package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/pricing/catalog"
)

var defaultOutputPath = filepath.FromSlash(
	"internal/pricing/snapshot/litellm_snapshot.json.gz",
)

func mustRate(dollars string) money.Money {
	rate, err := money.ParseDollars(dollars)
	if err != nil {
		panic(err)
	}
	return rate
}

const (
	defaultSnapshotRef      = "9b749891c4e15f302ffec7cd30029bbe5774cf84"
	defaultSnapshotSHA256   = "f899bb4d8f99cf19e4c63b929d8ba17eafef27e231c000af13bd3d0c8ba6e3d9"
	defaultSnapshotBranch   = "litellm-pricing-snapshot"
	defaultSnapshotFile     = "litellm_snapshot.json.gz"
	defaultSnapshotBaseURL  = "https://raw.githubusercontent.com/kenn-io/agentsview"
	defaultLiteLLMSourceRef = "418c7c6012d7c39a9d4a28c72cabe1995595ad2b"
)

var immutableGitRefPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

const (
	maxSnapshotCompressedBytes = 1 << 20
	maxSnapshotJSONBytes       = 8 << 20
	maxSnapshotModels          = 100_000
)

type snapshotBundle struct {
	Version   string                 `json:"version"`
	SourceRef string                 `json:"source_ref"`
	Models    []catalog.ModelPricing `json:"models"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	outPath := flag.String("out", defaultOutputPath, "output snapshot file path")
	validatePath := flag.String("validate", "", "validate a snapshot file and exit")
	restore := flag.Bool("restore", false, "restore a snapshot from a git artifact commit")
	restoreRef := flag.String("ref", defaultSnapshotRef, "git commit containing the snapshot artifact")
	restoreFile := flag.String("snapshot-file", defaultSnapshotFile, "snapshot path within the artifact commit")
	restoreSHA256 := flag.String("sha256", defaultSnapshotSHA256, "expected snapshot SHA256")
	restoreBranch := flag.String("branch", defaultSnapshotBranch, "artifact branch to fetch when ref is missing")
	restoreURL := flag.String("url", defaultSnapshotURL(), "snapshot URL to use when git restore is unavailable")
	litellmSourceRef := flag.String(
		"litellm-ref",
		defaultLiteLLMSourceRef,
		"immutable LiteLLM commit used to generate the snapshot",
	)
	flag.Parse()

	if *validatePath != "" {
		if err := validateSnapshotFile(*validatePath); err != nil {
			panic(err)
		}
		fmt.Printf("validated %s\n", *validatePath)
		return
	}
	if *restore {
		if err := restoreSnapshotFile(ctx,
			*outPath,
			*restoreRef,
			*restoreFile,
			*restoreSHA256,
			*restoreBranch,
			*restoreURL,
		); err != nil {
			panic(err)
		}
		return
	}

	if !immutableGitRefPattern.MatchString(*litellmSourceRef) {
		panic("litellm-ref must be a full lowercase commit SHA")
	}
	prices, err := catalog.FetchLiteLLMPricingAtRef(
		ctx,
		*litellmSourceRef,
	)
	if err != nil {
		panic(err)
	}

	prices = appendModelOverlay(prices)
	sort.Slice(prices, func(i, j int) bool {
		return prices[i].ModelPattern < prices[j].ModelPattern
	})

	modelsJSON, err := json.Marshal(prices)
	if err != nil {
		panic(err)
	}

	version := computeVersion(modelsJSON)
	bundle := snapshotBundle{
		Version:   version,
		SourceRef: *litellmSourceRef,
		Models:    prices,
	}

	raw, err := json.Marshal(bundle)
	if err != nil {
		panic(err)
	}

	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(raw); err != nil {
		panic(err)
	}
	if err := gz.Close(); err != nil {
		panic(err)
	}

	if err := os.WriteFile(*outPath, compressed.Bytes(), 0o644); err != nil {
		panic(err)
	}

	fmt.Printf("wrote %s\n", *outPath)
	fmt.Printf("snapshot version: %s\n", version)
	fmt.Printf("models: %d\n", len(prices))
}

func computeVersion(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "litellm-" + hex.EncodeToString(sum[:])[:12]
}

func defaultSnapshotURL() string {
	return defaultSnapshotBaseURL + "/" + defaultSnapshotRef + "/" + defaultSnapshotFile
}

func validateSnapshotFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat snapshot: %w", err)
	}
	if info.Size() == 0 {
		return errors.New("empty snapshot")
	}
	if info.Size() > maxSnapshotCompressedBytes {
		return fmt.Errorf(
			"compressed snapshot exceeds %d bytes",
			maxSnapshotCompressedBytes,
		)
	}

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening snapshot: %w", err)
	}
	defer file.Close()

	reader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("creating reader: %w", err)
	}
	defer reader.Close()

	raw, err := readLimitedSnapshotJSON(reader, maxSnapshotJSONBytes)
	if err != nil {
		return fmt.Errorf("decompressing snapshot: %w", err)
	}

	var snapshot snapshotBundle
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return fmt.Errorf("parsing snapshot json: %w", err)
	}
	if snapshot.Version == "" {
		return errors.New("missing snapshot version")
	}
	if !immutableGitRefPattern.MatchString(snapshot.SourceRef) {
		return errors.New("missing immutable LiteLLM source ref")
	}
	if len(snapshot.Models) == 0 {
		return errors.New("missing snapshot models")
	}
	if len(snapshot.Models) > maxSnapshotModels {
		return fmt.Errorf("snapshot models exceed %d entries", maxSnapshotModels)
	}
	for _, model := range snapshot.Models {
		if strings.TrimSpace(model.ModelPattern) == "" {
			return errors.New("snapshot contains model with empty pattern")
		}
		if err := catalog.NormalizePricingBands(model.ModelPattern, model.Bands); err != nil {
			return err
		}
	}

	return nil
}

func restoreSnapshotFile(ctx context.Context,
	outPath,
	ref,
	snapshotPath,
	expectedSHA256,
	branch,
	snapshotURL string,
) error {
	if ref == "" {
		return errors.New("missing artifact ref")
	}
	if snapshotPath == "" {
		return errors.New("missing artifact snapshot path")
	}
	if expectedSHA256 == "" {
		return errors.New("missing expected snapshot SHA256")
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("creating snapshot directory: %w", err)
	}

	if _, err := os.Stat(outPath); err == nil {
		actual, err := sha256File(outPath)
		if err != nil {
			return fmt.Errorf("hashing existing snapshot: %w", err)
		}
		if actual == expectedSHA256 {
			if err := validateSnapshotFile(outPath); err != nil {
				return fmt.Errorf("validating existing snapshot: %w", err)
			}
			fmt.Printf("Using existing %s\n", outPath)
			return nil
		}
		fmt.Printf(
			"Replacing %s: expected %s, got %s\n",
			outPath,
			expectedSHA256,
			actual,
		)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking existing snapshot: %w", err)
	}

	tmp := outPath + ".tmp"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale temp snapshot: %w", err)
	}
	defer os.Remove(tmp)

	if err := restoreSnapshotFileFromGit(ctx, tmp, ref, snapshotPath, branch); err != nil {
		if snapshotURL == "" {
			return err
		}
		if removeErr := os.Remove(tmp); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("removing failed git snapshot: %w", removeErr)
		}
		if downloadErr := downloadSnapshotFile(ctx, tmp, snapshotURL); downloadErr != nil {
			return fmt.Errorf(
				"restoring snapshot from git failed: %w; downloading snapshot failed: %w",
				err,
				downloadErr,
			)
		}
	}

	actual, err := sha256File(tmp)
	if err != nil {
		return fmt.Errorf("hashing restored snapshot: %w", err)
	}
	if actual != expectedSHA256 {
		return fmt.Errorf(
			"snapshot SHA256 mismatch: expected %s, got %s",
			expectedSHA256,
			actual,
		)
	}
	if err := validateSnapshotFile(tmp); err != nil {
		return fmt.Errorf("validating restored snapshot: %w", err)
	}

	if err := os.Rename(tmp, outPath); err != nil {
		if removeErr := os.Remove(outPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("replacing existing snapshot: %w", removeErr)
		}
		if renameErr := os.Rename(tmp, outPath); renameErr != nil {
			return fmt.Errorf("moving snapshot into place: %w", renameErr)
		}
	}

	fmt.Printf("Restored %s\n", outPath)
	return nil
}

func restoreSnapshotFileFromGit(ctx context.Context, tmp, ref, snapshotPath, branch string) error {
	if err := ensureGitCommit(ctx, ref, branch); err != nil {
		return err
	}

	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("creating temp snapshot: %w", err)
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", "show", ref+":"+snapshotPath)
	cmd.Stdout = file
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	closeErr := file.Close()
	if runErr != nil {
		return fmt.Errorf(
			"reading snapshot from git: %w: %s",
			runErr,
			strings.TrimSpace(stderr.String()),
		)
	}
	if closeErr != nil {
		return fmt.Errorf("closing temp snapshot: %w", closeErr)
	}

	return nil
}

func downloadSnapshotFile(ctx context.Context, tmp, snapshotURL string) error {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, snapshotURL, nil)
	if err != nil {
		return fmt.Errorf("creating snapshot request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("requesting snapshot: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("snapshot download returned status %d", resp.StatusCode)
	}

	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("creating temp snapshot: %w", err)
	}
	written, copyErr := io.Copy(file, io.LimitReader(resp.Body, maxSnapshotCompressedBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("writing downloaded snapshot: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing temp snapshot: %w", closeErr)
	}
	if written > maxSnapshotCompressedBytes {
		return fmt.Errorf(
			"downloaded snapshot exceeds %d bytes",
			maxSnapshotCompressedBytes,
		)
	}

	return nil
}

func ensureGitCommit(ctx context.Context, ref, branch string) error {
	if gitCommand(ctx, "cat-file", "-e", ref+"^{commit}") == nil {
		return nil
	}

	if err := fetchGitRef(ctx, ref); err == nil {
		if gitCommand(ctx, "cat-file", "-e", ref+"^{commit}") == nil {
			return nil
		}
	}

	if branch == "" {
		return fmt.Errorf("artifact ref %s is not available locally", ref)
	}

	if err := fetchGitRef(ctx, branch+":refs/remotes/origin/"+branch); err != nil {
		return err
	}
	if err := gitCommand(ctx, "cat-file", "-e", ref+"^{commit}"); err != nil {
		return fmt.Errorf("artifact ref %s is not available after fetch: %w", ref, err)
	}
	return nil
}

func fetchGitRef(ctx context.Context, refspec string) error {
	cmd := exec.CommandContext(ctx, "git", "fetch", "--depth=1", "origin", refspec)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"fetching snapshot ref %s: %w: %s",
			refspec,
			err,
			strings.TrimSpace(string(out)),
		)
	}
	return nil
}

func gitCommand(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	return cmd.Run()
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readLimitedSnapshotJSON(reader io.Reader, limit int64) ([]byte, error) {
	limited := io.LimitReader(reader, limit+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("decompressed snapshot exceeds %d bytes", limit)
	}
	return raw, nil
}

func appendModelOverlay(models []catalog.ModelPricing) []catalog.ModelPricing {
	present := make(map[string]struct{}, len(models))
	for _, price := range models {
		present[price.ModelPattern] = struct{}{}
	}

	overlay := map[string]catalog.ModelPricing{
		"claude-opus-4-6": {
			ModelPattern:           "claude-opus-4-6",
			InputPerMTok:           mustRate("5.0"),
			OutputPerMTok:          mustRate("25.0"),
			CacheCreationPerMTok:   mustRate("6.25"),
			CacheCreation1hPerMTok: mustRate("10.0"),
			CacheReadPerMTok:       mustRate("0.50"),
		},
		"claude-opus-4-7": {
			ModelPattern:           "claude-opus-4-7",
			InputPerMTok:           mustRate("5.0"),
			OutputPerMTok:          mustRate("25.0"),
			CacheCreationPerMTok:   mustRate("6.25"),
			CacheCreation1hPerMTok: mustRate("10.0"),
			CacheReadPerMTok:       mustRate("0.50"),
		},
		"claude-opus-4-8": {
			ModelPattern:           "claude-opus-4-8",
			InputPerMTok:           mustRate("5.0"),
			OutputPerMTok:          mustRate("25.0"),
			CacheCreationPerMTok:   mustRate("6.25"),
			CacheCreation1hPerMTok: mustRate("10.0"),
			CacheReadPerMTok:       mustRate("0.50"),
		},
		"claude-opus-4-20250514": {
			ModelPattern:           "claude-opus-4-20250514",
			InputPerMTok:           mustRate("15.0"),
			OutputPerMTok:          mustRate("75.0"),
			CacheCreationPerMTok:   mustRate("18.75"),
			CacheCreation1hPerMTok: mustRate("30.0"),
			CacheReadPerMTok:       mustRate("1.50"),
		},
		"claude-fable-5": {
			ModelPattern:           "claude-fable-5",
			InputPerMTok:           mustRate("10.0"),
			OutputPerMTok:          mustRate("50.0"),
			CacheCreationPerMTok:   mustRate("12.50"),
			CacheCreation1hPerMTok: mustRate("20.0"),
			CacheReadPerMTok:       mustRate("1.00"),
		},
		"claude-sonnet-4-6": {
			ModelPattern:           "claude-sonnet-4-6",
			InputPerMTok:           mustRate("3.0"),
			OutputPerMTok:          mustRate("15.0"),
			CacheCreationPerMTok:   mustRate("3.75"),
			CacheCreation1hPerMTok: mustRate("6.0"),
			CacheReadPerMTok:       mustRate("0.30"),
		},
		"claude-sonnet-4-20250514": {
			ModelPattern:           "claude-sonnet-4-20250514",
			InputPerMTok:           mustRate("3.0"),
			OutputPerMTok:          mustRate("15.0"),
			CacheCreationPerMTok:   mustRate("3.75"),
			CacheCreation1hPerMTok: mustRate("6.0"),
			CacheReadPerMTok:       mustRate("0.30"),
		},
		"claude-sonnet-4-5-20250514": {
			ModelPattern:           "claude-sonnet-4-5-20250514",
			InputPerMTok:           mustRate("3.0"),
			OutputPerMTok:          mustRate("15.0"),
			CacheCreationPerMTok:   mustRate("3.75"),
			CacheCreation1hPerMTok: mustRate("6.0"),
			CacheReadPerMTok:       mustRate("0.30"),
		},
		"claude-haiku-4-5-20251001": {
			ModelPattern:           "claude-haiku-4-5-20251001",
			InputPerMTok:           mustRate("1.0"),
			OutputPerMTok:          mustRate("5.0"),
			CacheCreationPerMTok:   mustRate("1.25"),
			CacheCreation1hPerMTok: mustRate("2.0"),
			CacheReadPerMTok:       mustRate("0.10"),
		},
		"claude-haiku-3-5-20241022": {
			ModelPattern:           "claude-haiku-3-5-20241022",
			InputPerMTok:           mustRate("0.80"),
			OutputPerMTok:          mustRate("4.0"),
			CacheCreationPerMTok:   mustRate("1.0"),
			CacheCreation1hPerMTok: mustRate("1.6"),
			CacheReadPerMTok:       mustRate("0.08"),
		},
		"gpt-5.5": {
			ModelPattern:     "gpt-5.5",
			InputPerMTok:     mustRate("5.0"),
			OutputPerMTok:    mustRate("30.0"),
			CacheReadPerMTok: mustRate("0.50"),
		},
		"gpt-5.4": {
			ModelPattern:     "gpt-5.4",
			InputPerMTok:     mustRate("2.50"),
			OutputPerMTok:    mustRate("15.0"),
			CacheReadPerMTok: mustRate("0.25"),
		},
		"gpt-5.4-mini": {
			ModelPattern:     "gpt-5.4-mini",
			InputPerMTok:     mustRate("0.75"),
			OutputPerMTok:    mustRate("4.50"),
			CacheReadPerMTok: mustRate("0.075"),
		},
		"gpt-5.4-nano": {
			ModelPattern:     "gpt-5.4-nano",
			InputPerMTok:     mustRate("0.20"),
			OutputPerMTok:    mustRate("1.25"),
			CacheReadPerMTok: mustRate("0.02"),
		},
		"gpt-5.3-codex": {
			ModelPattern:     "gpt-5.3-codex",
			InputPerMTok:     mustRate("1.75"),
			OutputPerMTok:    mustRate("14.0"),
			CacheReadPerMTok: mustRate("0.175"),
		},
		"gpt-5.2-codex": {
			ModelPattern:     "gpt-5.2-codex",
			InputPerMTok:     mustRate("1.75"),
			OutputPerMTok:    mustRate("14.0"),
			CacheReadPerMTok: mustRate("0.175"),
		},
		"gpt-5.1-codex-max": {
			ModelPattern:     "gpt-5.1-codex-max",
			InputPerMTok:     mustRate("1.25"),
			OutputPerMTok:    mustRate("10.0"),
			CacheReadPerMTok: mustRate("0.125"),
		},
		"mistral-large": {
			ModelPattern:  "mistral-large",
			InputPerMTok:  mustRate("4.0"),
			OutputPerMTok: mustRate("4.0"),
		},
		"mistral-large-3": {
			ModelPattern:         "mistral-large-3",
			InputPerMTok:         mustRate("4.0"),
			OutputPerMTok:        mustRate("4.0"),
			CacheCreationPerMTok: mustRate("4.0"),
			CacheReadPerMTok:     mustRate("0.30"),
		},
		"mistral-medium": {
			ModelPattern:  "mistral-medium",
			InputPerMTok:  mustRate("2.75"),
			OutputPerMTok: mustRate("2.75"),
		},
		"mistral-medium-3": {
			ModelPattern:  "mistral-medium-3",
			InputPerMTok:  mustRate("2.75"),
			OutputPerMTok: mustRate("2.75"),
		},
		"mistral-medium-3.5": {
			ModelPattern:         "mistral-medium-3.5",
			InputPerMTok:         mustRate("1.5"),
			OutputPerMTok:        mustRate("7.5"),
			CacheCreationPerMTok: mustRate("1.5"),
			CacheReadPerMTok:     mustRate("0.25"),
		},
		"openrouter/owl-alpha": {
			ModelPattern:  "openrouter/owl-alpha",
			InputPerMTok:  mustRate("0"),
			OutputPerMTok: mustRate("0"),
		},
	}

	out := slices.Clone(models)
	for modelPattern, price := range overlay {
		if _, ok := present[modelPattern]; ok {
			continue
		}
		out = append(out, price)
	}
	return out
}
