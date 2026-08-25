package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"go.kenn.io/agentsview/internal/pricing/catalog"
)

const defaultGenAISourceRef = "83a49e8b386176a1e28e9d9aedeea5e2b4abc586"

var immutableGitRefPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type snapshotBundle struct {
	Version   string         `json:"version"`
	SourceRef string         `json:"source_ref"`
	Data      jsontext.Value `json:"data"`
}

func main() {
	outPath := flag.String(
		"out",
		filepath.FromSlash("internal/pricing/snapshot/genai_prices.json.gz"),
		"output snapshot file path",
	)
	sourceRef := flag.String(
		"genai-ref", defaultGenAISourceRef,
		"immutable GenAI Prices commit used to generate the snapshot",
	)
	flag.Parse()
	if !immutableGitRefPattern.MatchString(*sourceRef) {
		panic("genai-ref must be a full lowercase commit SHA")
	}
	prices, err := catalog.FetchGenAIPricesAtRef(
		context.Background(), *sourceRef,
	)
	if err != nil {
		panic(err)
	}
	normalizedData, err := json.Marshal(jsontext.Value(prices.RawJSON()))
	if err != nil {
		panic(err)
	}
	prices, err = catalog.ParseGenAIPrices(normalizedData)
	if err != nil {
		panic(err)
	}
	bundle := snapshotBundle{
		Version: prices.Version(), SourceRef: *sourceRef,
		Data: jsontext.Value(normalizedData),
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		panic(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(raw); err != nil {
		panic(err)
	}
	if err := writer.Close(); err != nil {
		panic(err)
	}
	if err := os.WriteFile(*outPath, compressed.Bytes(), 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("wrote %s\n", *outPath)
	fmt.Printf("snapshot version: %s\n", prices.Version())
}
