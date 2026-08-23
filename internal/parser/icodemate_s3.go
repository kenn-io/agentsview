package parser

import (
	"path"
	"strings"
)

// ICodeMate's terminal CLI stores Claude-layout transcripts, so its S3
// surface reuses Claude's scanner, sidecar-aware stat, and temp-path rules
// under the icodemate provider segment (.../<machine>/raw/icodemate/...) and
// the icodemate: session namespace. The OpenCode-storage family never ingests
// from S3; only CLI transcripts are kept.

var _ S3Provider = (*icodemateProvider)(nil)

func icodemateCLIS3Scanner() S3SessionScanner {
	scanner := claudeS3Scanner()
	scanner.Agent = AgentIcodemate
	return scanner
}

func (p *icodemateProvider) S3Scanner() S3SessionScanner {
	return icodemateCLIS3Scanner()
}

func (p *icodemateProvider) S3SessionID(uri string) string {
	id, ok := strings.CutSuffix(path.Base(uri), ".jsonl")
	if !ok || id == "" {
		return ""
	}
	return "icodemate:" + id
}

func (p *icodemateProvider) S3TempRelPath(objectPath string) (string, error) {
	return s3TempRelPathAfterRawAgent(objectPath, string(AgentIcodemate), nil)
}

func (p *icodemateProvider) S3StatSession(uri string) (S3Object, error) {
	return StatClaudeS3Session(uri)
}

func (p *icodemateProvider) S3PostFetchHydrate(
	tempDir, tempPath, configuredRoot, objectURI string,
) error {
	return nil
}
