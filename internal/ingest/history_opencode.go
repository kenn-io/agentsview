package ingest

import (
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/usagefacts"
)

func shouldPreserveOpenCodeFormatArchive(
	agent parser.AgentType,
	candidate Candidate,
	prior PriorSession,
	options ContentOptions,
) bool {
	path := candidate.Parsed.Session.File.Path
	if path == "" {
		path = derefString(candidate.Session.FilePath)
	}
	if IsClaudeFormatTranscript(agent, path) {
		return false
	}
	storedHash := derefString(prior.Session.FileHash)
	storedPath := derefString(prior.Session.FilePath)
	storedMtime := derefInt64(prior.Session.FileMtime)
	currentMtime := candidate.Parsed.Session.File.Mtime
	if currentMtime == 0 {
		currentMtime = derefInt64(candidate.Session.FileMtime)
	}
	currentHash := candidate.Parsed.Session.File.Hash
	if currentHash == "" {
		currentHash = derefString(candidate.Session.FileHash)
	}
	storedHasStorageFingerprint := parser.HasOpenCodeStorageFingerprint(storedHash)
	currentIsStorage := IsOpenCodeFormatStoragePath(agent, path) ||
		IsOpenCodeFormatSQLiteVirtualPath(agent, path)
	storedIsStorage := IsOpenCodeFormatStoragePath(agent, storedPath) ||
		IsOpenCodeFormatSQLiteVirtualPath(agent, storedPath) ||
		(storedPath == "" && storedHasStorageFingerprint)
	if !currentIsStorage && !storedIsStorage {
		return false
	}
	storedIsSQLiteVirtual := IsOpenCodeFormatSQLiteVirtualPath(agent, storedPath)
	storedIsStorageArchive := IsOpenCodeFormatStoragePath(agent, storedPath) ||
		(storedPath == "" && storedHasStorageFingerprint)
	if storedIsSQLiteVirtual {
		storedIsStorageArchive = false
	}
	if IsOpenCodeFormatSQLiteVirtualPath(agent, path) && !storedIsStorageArchive {
		return false
	}
	if len(prior.Messages) == 0 {
		return false
	}
	if storedHasStorageFingerprint &&
		parser.HasOpenCodeStorageFingerprint(currentHash) &&
		!parser.OpenCodeStorageFingerprintMissing(storedHash, currentHash) {
		return false
	}
	if storedIsStorageArchive &&
		IsOpenCodeFormatSQLiteVirtualPath(agent, path) &&
		currentMtime != 0 && storedMtime != 0 && currentMtime <= storedMtime {
		return true
	}
	if options.ArchiveContent.UsageOnly() {
		_, currentMessages := db.ProjectSessionForStoragePolicy(
			db.Session{}, candidate.Messages, options.ArchiveContent,
		)
		_, storedMessages := db.ProjectSessionForStoragePolicy(
			db.Session{}, prior.Messages, options.ArchiveContent,
		)
		return OpenCodeUsageOnlyArchiveLooksIncomplete(
			currentMessages, storedMessages,
		)
	}
	return OpenCodeLegacyArchiveLooksIncomplete(
		candidate.Messages, prior.Messages,
	)
}

// IsClaudeFormatTranscript reports whether path is a session-bearing Claude
// projects-layout transcript for an agent that uses the shared DAG parser.
func IsClaudeFormatTranscript(agent parser.AgentType, path string) bool {
	if !IsClaudeFormatAgent(agent) {
		return false
	}
	name := filepath.Base(path)
	return name != ".jsonl" && strings.HasSuffix(name, ".jsonl")
}

// IsClaudeFormatAgent reports agents that use the shared Claude DAG parser.
func IsClaudeFormatAgent(agent parser.AgentType) bool {
	return agent == parser.AgentClaude || agent == parser.AgentIcodemate
}

// IsOpenCodeFormatStorageAgent reports agents that consume OpenCode storage.
func IsOpenCodeFormatStorageAgent(agent parser.AgentType) bool {
	return agent == parser.AgentOpenCode ||
		agent == parser.AgentKilo ||
		agent == parser.AgentIcodemate ||
		agent == parser.AgentMiMoCode
}

// OpenCodeFormatDBName returns the provider's OpenCode-format container name.
func OpenCodeFormatDBName(agent parser.AgentType) string {
	switch agent {
	case parser.AgentOpenCode:
		return "opencode.db"
	case parser.AgentKilo:
		return "kilo.db"
	case parser.AgentMiMoCode:
		return "mimocode.db"
	case parser.AgentIcodemate:
		return "icodemate.db"
	default:
		return ""
	}
}

// IsOpenCodeFormatStoragePath reports a JSON storage member path.
func IsOpenCodeFormatStoragePath(agent parser.AgentType, path string) bool {
	return strings.HasSuffix(path, ".json") &&
		!IsOpenCodeFormatSQLiteVirtualPath(agent, path)
}

// IsOpenCodeFormatContainerSource reports a storage member or SQLite member.
func IsOpenCodeFormatContainerSource(agent parser.AgentType, path string) bool {
	return IsOpenCodeFormatStorageAgent(agent) &&
		(IsOpenCodeFormatStoragePath(agent, path) ||
			IsOpenCodeFormatSQLiteVirtualPath(agent, path))
}

// IsOpenCodeFormatSQLiteVirtualPath reports a provider container member path.
func IsOpenCodeFormatSQLiteVirtualPath(
	agent parser.AgentType, path string,
) bool {
	if !IsOpenCodeFormatStorageAgent(agent) {
		return false
	}
	if agent == parser.AgentOpenCode {
		_, _, ok := parser.ParseOpenCodeSQLiteVirtualPath(path)
		return ok
	}
	_, _, ok := parser.ParseVirtualSourcePathForBase(
		path, OpenCodeFormatDBName(agent),
	)
	return ok
}

// OpenCodeLegacyArchiveLooksIncomplete compares full-content rows using the
// historical OpenCode archive rules.
func OpenCodeLegacyArchiveLooksIncomplete(parsed, stored []db.Message) bool {
	if parsed == nil {
		return len(stored) > 0
	}
	if len(parsed) < len(stored) {
		return true
	}
	for i := range stored {
		if openCodeMessageLooksIncomplete(parsed[i], stored[i]) {
			return true
		}
	}
	return false
}

// OpenCodeUsageOnlyArchiveLooksIncomplete compares the fields retained by the
// usage-only storage policy, including stable message and delegation identity.
func OpenCodeUsageOnlyArchiveLooksIncomplete(parsed, stored []db.Message) bool {
	if parsed == nil {
		return len(stored) > 0
	}
	if len(parsed) < len(stored) {
		return true
	}
	parsedByIdentity := make(map[openCodeMessageIdentity]db.Message, len(parsed)*2)
	for _, message := range parsed {
		parsedByIdentity[openCodeMessageStorageIdentity(message)] = message
		parsedByIdentity[openCodeMessageIdentity{
			ordinal: message.Ordinal, role: message.Role,
		}] = message
	}
	for _, storedMessage := range stored {
		identity := openCodeMessageStorageIdentity(storedMessage)
		parsedMessage, ok := parsedByIdentity[identity]
		if !ok || openCodeUsageOnlyMessageLooksIncomplete(
			parsedMessage, storedMessage,
		) {
			return true
		}
	}
	return false
}

type openCodeMessageIdentity struct {
	sourceUUID string
	ordinal    int
	role       string
}

func openCodeMessageStorageIdentity(message db.Message) openCodeMessageIdentity {
	if message.SourceUUID != "" {
		return openCodeMessageIdentity{sourceUUID: message.SourceUUID}
	}
	return openCodeMessageIdentity{ordinal: message.Ordinal, role: message.Role}
}

func openCodeMessageLooksIncomplete(parsed, stored db.Message) bool {
	if parsed.Ordinal != stored.Ordinal || parsed.Role != stored.Role {
		return false
	}
	if sanitizedMessageContentLength(parsed) < sanitizedMessageContentLength(stored) {
		return true
	}
	if parsed.HasThinking != stored.HasThinking && stored.HasThinking {
		return true
	}
	if stored.HasOutputTokens &&
		(!parsed.HasOutputTokens || parsed.OutputTokens < stored.OutputTokens) {
		return true
	}
	if stored.HasContextTokens &&
		(!parsed.HasContextTokens || parsed.ContextTokens < stored.ContextTokens) {
		return true
	}
	if len(parsed.ToolCalls) < len(stored.ToolCalls) {
		return true
	}
	return countToolResultEvents(parsed.ToolCalls) <
		countToolResultEvents(stored.ToolCalls)
}

func openCodeUsageOnlyMessageLooksIncomplete(parsed, stored db.Message) bool {
	if parsed.Role != stored.Role {
		return false
	}
	if openCodeUsageLooksIncomplete(parsed, stored) {
		return true
	}
	parsedLinks := make(map[string]string, len(parsed.ToolCalls))
	for _, call := range parsed.ToolCalls {
		parsedLinks[call.ToolUseID] = call.SubagentSessionID
	}
	for _, call := range stored.ToolCalls {
		child, ok := parsedLinks[call.ToolUseID]
		if !ok || child != call.SubagentSessionID {
			return true
		}
	}
	return false
}

func openCodeUsageLooksIncomplete(parsed, stored db.Message) bool {
	if stored.Model != "" && parsed.Model == "" {
		return true
	}
	if len(stored.TokenUsage) == 0 {
		return false
	}
	if len(parsed.TokenUsage) == 0 {
		return true
	}
	parsedUsage := usagefacts.ParseTokenUsage(string(parsed.TokenUsage))
	storedUsage := usagefacts.ParseTokenUsage(string(stored.TokenUsage))
	return parsedUsage.InputTokens < storedUsage.InputTokens ||
		parsedUsage.OutputTokens < storedUsage.OutputTokens ||
		parsedUsage.ReasoningTokens < storedUsage.ReasoningTokens ||
		parsedUsage.CacheCreationTokens < storedUsage.CacheCreationTokens ||
		parsedUsage.CacheCreation1hTokens < storedUsage.CacheCreation1hTokens ||
		parsedUsage.CacheReadTokens < storedUsage.CacheReadTokens ||
		parsedUsage.WebSearchRequests < storedUsage.WebSearchRequests
}

func sanitizedMessageContentLength(message db.Message) int {
	sanitized := db.SanitizeUTF8(message.Content)
	if sanitized != message.Content {
		return len(sanitized)
	}
	return message.ContentLength
}

func countToolResultEvents(calls []db.ToolCall) int {
	total := 0
	for _, call := range calls {
		total += len(call.ResultEvents)
	}
	return total
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
