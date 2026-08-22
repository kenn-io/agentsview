package capture

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/pricingrefresh"
	"go.kenn.io/agentsview/internal/service"
	syncer "go.kenn.io/agentsview/internal/sync"
)

type sourceSnapshot struct {
	Path    string
	Size    int64
	ModTime time.Time
}

type codexSourceSelection struct {
	ID       string
	Anchor   time.Time
	LivePath string
}

func rejectExistingClaudeSources(root, sessionID string, limits Limits) error {
	dir, err := os.Open(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening Claude project root: %w", err)
	}
	defer dir.Close()
	examined := 0
	for {
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			if entry.Name() == claudeReservationDirName {
				continue
			}
			examined++
			if examined > limits.MaxSources*32 {
				return errorWithReason(
					ReasonSourceLimit,
					"Claude project root exceeds candidate limit",
				)
			}
			if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
				continue
			}
			if err := rejectClaudeSessionIDInShard(
				root, entry.Name(), sessionID,
			); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("listing Claude project root: %w", readErr)
		}
	}
}

func rejectClaudeSessionIDInShard(root, shard, sessionID string) error {
	base := filepath.Join(root, shard, sessionID)
	for _, candidate := range []string{base + ".jsonl", base} {
		if _, err := os.Lstat(candidate); err == nil {
			return errors.New(
				"claude session ID already has provider source data; choose a new UUID",
			)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("checking existing Claude session source: %w", err)
		}
	}
	return nil
}

func locateRoot(ctx context.Context, m manifest) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch Provider(m.Provider) {
	case ProviderClaude:
		path := filepath.Join(
			m.ProviderRoot, encodeClaudeWorkDir(m.ProviderWorkDir),
			m.ProviderSessionID+".jsonl",
		)
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
		return []string{path}, nil
	case ProviderCodex:
		return locateCodexRoot(
			ctx, m.ProviderRoot, m.ProviderSessionID, m.StartedAt, m.Limits)
	default:
		return nil, fmt.Errorf("unsupported provider %q", m.Provider)
	}
}

func locateCodexRoot(
	ctx context.Context,
	root, id string,
	started time.Time,
	limits Limits,
) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validUUID(id) {
		return nil, nil
	}
	days := make(map[string]struct{})
	for offset := -1; offset <= 1; offset++ {
		localDay := started.AddDate(0, 0, offset)
		utcDay := started.UTC().AddDate(0, 0, offset)
		days[localDay.Format("2006/01/02")] = struct{}{}
		days[utcDay.Format("2006/01/02")] = struct{}{}
	}
	ordered := make([]string, 0, len(days))
	for day := range days {
		ordered = append(ordered, day)
	}
	sort.Strings(ordered)
	var matches []string
	examined := 0
	for _, day := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dir := filepath.Join(root, filepath.FromSlash(day))
		f, err := os.Open(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for {
			if err := ctx.Err(); err != nil {
				f.Close()
				return nil, err
			}
			entries, readErr := f.ReadDir(128)
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					f.Close()
					return nil, err
				}
				examined++
				if examined > limits.MaxSources*32 {
					f.Close()
					return nil, errorWithReason(
						ReasonSourceLimit,
						"Codex date shard exceeds candidate limit",
					)
				}
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), "-"+id+".jsonl") {
					continue
				}
				path := filepath.Join(dir, entry.Name())
				matched, matchErr := codexMetaMatches(
					ctx, path, id, limits.MaxLineBytes)
				if matchErr != nil {
					f.Close()
					return nil, matchErr
				}
				if matched {
					matches = append(matches, path)
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				f.Close()
				return nil, readErr
			}
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	}
	sort.Strings(matches)
	return matches, nil
}

func codexMetaMatches(
	ctx context.Context,
	path, id string,
	maxLine int,
) (bool, error) {
	line, err := scanFirstLine(ctx, path, maxLine)
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var record struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	decodeErr := json.Unmarshal(line, &record)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if decodeErr != nil {
		return false, nil
	}
	return record.Type == "session_meta" && strings.EqualFold(record.Payload.ID, id), nil
}

func claudeSources(ctx context.Context, rootPath string, limits Limits) ([]string, error) {
	paths := []string{rootPath}
	subagents := strings.TrimSuffix(rootPath, ".jsonl")
	subagents = filepath.Join(subagents, "subagents")
	if _, err := os.Stat(subagents); errors.Is(err, os.ErrNotExist) {
		return paths, nil
	} else if err != nil {
		return nil, err
	}
	examined := 0
	err := filepath.WalkDir(subagents, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		examined++
		if examined > limits.MaxSources*8 {
			return errorWithReason(
				ReasonSourceLimit,
				"Claude subagent tree exceeds entry limit",
			)
		}
		if entry.Type()&os.ModeSymlink != 0 && entry.IsDir() {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "agent-") ||
			!strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		if len(paths) >= limits.MaxSources {
			return errorWithReason(ReasonSourceLimit, "source count exceeds limit")
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths[1:])
	return paths, nil
}

func snapshotSources(
	ctx context.Context,
	paths []string,
	limits Limits,
) ([]sourceSnapshot, error) {
	if len(paths) == 0 || len(paths) > limits.MaxSources {
		return nil, errorWithReason(ReasonSourceLimit, "source count exceeds limit")
	}
	var total int64
	out := make([]sourceSnapshot, 0, len(paths))
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("capture source is not a regular file")
		}
		if info.Size() > limits.MaxSourceBytes {
			return nil, errorWithReason(
				ReasonSourceBytesLimit,
				"capture source exceeds per-file byte limit",
			)
		}
		total += info.Size()
		if total > limits.MaxTotalBytes {
			return nil, errorWithReason(
				ReasonSourceBytesLimit,
				"capture sources exceed aggregate byte limit",
			)
		}
		if err := validateJSONLLines(ctx, path, limits.MaxLineBytes); err != nil {
			return nil, err
		}
		out = append(out, sourceSnapshot{Path: path, Size: info.Size(), ModTime: info.ModTime()})
	}
	return out, nil
}

func validateJSONLLines(ctx context.Context, path string, maxLineBytes int) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, min(maxLineBytes, 64<<10))
	lineBytes := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, readErr := reader.ReadSlice('\n')
		lineBytes += len(chunk)
		if lineBytes > maxLineBytes {
			return errorWithReason(
				ReasonSourceBytesLimit,
				"capture source JSONL line exceeds byte limit",
			)
		}
		switch {
		case readErr == nil:
			lineBytes = 0
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF):
			return nil
		default:
			return readErr
		}
	}
}

func sameSnapshots(a, b []sourceSnapshot) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Path != b[i].Path || a[i].Size != b[i].Size ||
			!a[i].ModTime.Equal(b[i].ModTime) {
			return false
		}
	}
	return true
}

type ingestedCapture struct {
	Database      *db.DB
	Root          *db.Session
	Usage         *db.SessionUsage
	Descendants   []db.Session
	Paths         []string
	LiveSnapshots []sourceSnapshot
	CodexSources  []codexSourceSelection
}

func ingest(
	ctx context.Context,
	state *captureState,
	paths []string,
	liveSnapshots []sourceSnapshot,
	customPricing map[string]config.CustomModelRate,
	deadline time.Time,
	discoverCodexChildren bool,
) (*ingestedCapture, error) {
	database, engine, err := openCaptureEngine(ctx, state, customPricing)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*ingestedCapture, error) {
		engine.Close()
		return nil, errors.Join(err, database.CloseContext(ctx))
	}
	if err := engine.SyncPathsContext(ctx, paths); err != nil {
		return fail(err)
	}
	allPaths := append([]string(nil), paths...)
	allSnapshots := append([]sourceSnapshot(nil), liveSnapshots...)
	var codexSources []codexSourceSelection
	if Provider(state.manifest.Provider) == ProviderCodex && len(liveSnapshots) == 1 {
		codexSources = append(codexSources, codexSourceSelection{
			ID: state.manifest.ProviderSessionID, Anchor: state.manifest.StartedAt,
			LivePath: liveSnapshots[0].Path,
		})
	}
	if Provider(state.manifest.Provider) == ProviderCodex && discoverCodexChildren {
		newPaths, newSnapshots, selections, childErr := ingestCodexChildren(
			ctx, state, database, engine, deadline)
		if childErr != nil {
			return fail(childErr)
		}
		allPaths = append(allPaths, newPaths...)
		allSnapshots = append(allSnapshots, newSnapshots...)
		codexSources = append(codexSources, selections...)
	}
	root, usage, descendants, err := loadCapturedUsage(ctx, state, database)
	if err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	engine.Close()
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return &ingestedCapture{
		Database: database, Root: root, Usage: usage,
		Descendants: descendants, Paths: allPaths, LiveSnapshots: allSnapshots,
		CodexSources: codexSources,
	}, nil
}

func openCaptureEngine(
	ctx context.Context,
	state *captureState,
	customPricing map[string]config.CustomModelRate,
) (*db.DB, *syncer.Engine, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	// Provider copies are the authoritative recovery evidence. Rebuild the
	// disposable archive on every attempt so a canceled initialization cannot
	// leave schema or rows that affect a later report.
	if err := state.removeAttemptArchive(); err != nil {
		return nil, nil, err
	}
	if err := prepareCaptureArchive(state.archivePath()); err != nil {
		return nil, nil, err
	}
	database, err := db.OpenFreshIsolatedContext(ctx, state.archivePath())
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*db.DB, *syncer.Engine, error) {
		return nil, nil, errors.Join(err, database.CloseContext(ctx))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := pricingrefresh.SeedFallbackContext(ctx, database); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	database.SetCustomPricing(customPricing)
	agent := parser.AgentType(state.manifest.Provider)
	disabled := make([]parser.AgentType, 0, len(parser.Registry)-1)
	for _, def := range parser.Registry {
		if def.Type != agent {
			disabled = append(disabled, def.Type)
		}
	}
	engine := syncer.NewEngine(database, syncer.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			agent: {state.capturedProviderRoot()},
		},
		DisabledAgents:                    disabled,
		Machine:                           "capture",
		Ephemeral:                         true,
		DiscardPendingWritesOnCancel:      true,
		DisableSignalRecomputation:        true,
		DisableFilesystemProjectDiscovery: true,
		StableSourceSnapshots:             true,
	})
	if err := ctx.Err(); err != nil {
		engine.Close()
		return fail(err)
	}
	return database, engine, nil
}

func prepareCaptureArchive(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("closing private capture archive: %w", closeErr)
		}
		return nil
	}
	if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("creating private capture archive: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("checking capture archive: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("capture archive is not a regular file")
	}
	if err := verifyCapturePathOwner(path); err != nil {
		return fmt.Errorf("validating capture archive owner: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("securing capture archive: %w", err)
	}
	return nil
}

func ingestCodexChildren(
	ctx context.Context,
	state *captureState,
	database *db.DB,
	engine *syncer.Engine,
	deadline time.Time,
) ([]string, []sourceSnapshot, []codexSourceSelection, error) {
	rootID := "codex:" + state.manifest.ProviderSessionID
	frontier := []string{rootID}
	seen := map[string]bool{rootID: true}
	var allPaths []string
	var allSnapshots []sourceSnapshot
	var selections []codexSourceSelection
	for len(seen) <= state.manifest.Limits.MaxSources {
		children, err := codexChildRefs(ctx, database, frontier)
		if err != nil {
			return nil, nil, nil, err
		}
		pending := unseenCodexChildren(children, seen)
		if len(pending) == 0 {
			return allPaths, allSnapshots, selections, nil
		}
		if len(seen)+len(pending) > state.manifest.Limits.MaxSources {
			return nil, nil, nil, errorWithReason(
				ReasonSourceLimit, "source count exceeds limit")
		}
		livePaths, err := awaitCodexChildPaths(ctx, state, pending, deadline)
		if err != nil {
			return allPaths, allSnapshots, selections, err
		}
		stable, err := awaitStablePaths(
			ctx, livePaths, deadline, state.manifest.Limits)
		if err != nil {
			return nil, nil, nil, err
		}
		persisted, err := state.persistSourcePaths(ctx, stable)
		if err != nil {
			return nil, nil, nil, err
		}
		if err := engine.SyncPathsContext(ctx, persisted.Paths); err != nil {
			return nil, nil, nil, err
		}
		frontier = frontier[:0]
		for i, child := range pending {
			seen[child.ID] = true
			frontier = append(frontier, child.ID)
			selections = append(selections, codexSourceSelection{
				ID:     strings.TrimPrefix(child.ID, "codex:"),
				Anchor: child.SpawnedAt, LivePath: livePaths[i],
			})
		}
		allPaths = append(allPaths, persisted.Paths...)
		allSnapshots = append(allSnapshots, persisted.Snapshots...)
	}
	return nil, nil, nil, errorWithReason(ReasonSourceLimit, "source count exceeds limit")
}

type codexChildRef struct {
	ID        string
	SpawnedAt time.Time
}

func codexChildRefs(
	ctx context.Context,
	database *db.DB,
	parents []string,
) ([]codexChildRef, error) {
	byID := make(map[string]time.Time)
	for _, parent := range parents {
		messages, err := database.GetAllMessages(ctx, parent)
		if err != nil {
			return nil, err
		}
		for _, message := range messages {
			for _, call := range message.ToolCalls {
				child := call.SubagentSessionID
				if child == "" || child == parent {
					continue
				}
				spawnedAt, err := time.Parse(time.RFC3339Nano, message.Timestamp)
				if err != nil {
					return nil, errorWithReason(
						ReasonSourceUnavailable,
						"Codex child spawn timestamp is unavailable",
					)
				}
				if prior, ok := byID[child]; !ok || spawnedAt.Before(prior) {
					byID[child] = spawnedAt
				}
			}
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	refs := make([]codexChildRef, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, codexChildRef{ID: id, SpawnedAt: byID[id]})
	}
	return refs, nil
}

func unseenCodexChildren(
	children []codexChildRef,
	seen map[string]bool,
) []codexChildRef {
	var pending []codexChildRef
	for _, child := range children {
		if !seen[child.ID] {
			pending = append(pending, child)
		}
	}
	return pending
}

func awaitCodexChildPaths(
	ctx context.Context,
	state *captureState,
	children []codexChildRef,
	deadline time.Time,
) ([]string, error) {
	for {
		paths := make([]string, 0, len(children))
		for _, child := range children {
			raw := strings.TrimPrefix(child.ID, "codex:")
			matches, err := locateCodexRoot(
				ctx, state.manifest.ProviderRoot, raw,
				child.SpawnedAt, state.manifest.Limits,
			)
			if err != nil {
				return nil, err
			}
			if len(matches) > 1 {
				return nil, errorWithReason(
					ReasonMultipleSessions, "multiple exact Codex child sources found")
			}
			if len(matches) == 0 {
				paths = nil
				break
			}
			paths = append(paths, matches[0])
		}
		if len(paths) == len(children) {
			return paths, nil
		}
		if err := waitForPoll(ctx, deadline); err != nil {
			return nil, &reasonError{
				reason: ReasonSourceUnavailable,
				err:    fmt.Errorf("exact Codex child source is unavailable: %w", err),
			}
		}
	}
}

func loadCapturedUsage(
	ctx context.Context,
	state *captureState,
	database *db.DB,
) (*db.Session, *db.SessionUsage, []db.Session, error) {
	rootID := state.manifest.ProviderSessionID
	if Provider(state.manifest.Provider) == ProviderCodex {
		rootID = "codex:" + rootID
	}
	root, err := database.GetSession(ctx, rootID)
	if err != nil || root == nil {
		if err == nil {
			err = errors.New("exact provider session was not ingested")
		}
		return nil, nil, nil, err
	}
	var usage *db.SessionUsage
	var descendants []db.Session
	switch Provider(state.manifest.Provider) {
	case ProviderClaude:
		required := capturedClaudeSubagentIDs(state.manifest.Sources, rootID)
		usage, descendants, err = service.SessionUsageWithRequiredSubagents(
			ctx, database, rootID, required, true,
		)
		if err == nil {
			err = validateClaudeSubagentReferences(
				ctx, database, rootID, descendants, required,
			)
		}
	case ProviderCodex:
		required := capturedCodexChildIDs(state.manifest.Sources, rootID)
		usage, descendants, err = service.SessionUsageWithRequiredSubagents(
			ctx, database, rootID, required, true,
		)
	}
	if err != nil || usage == nil {
		if err == nil {
			err = errors.New("usage is unavailable")
		}
		return nil, nil, nil, err
	}
	return root, usage, descendants, nil
}

func capturedCodexChildIDs(sources []TranscriptSource, rootID string) []string {
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		id := "codex:" + source.SessionID
		if source.SessionID == "" || id == rootID {
			continue
		}
		seen[id] = struct{}{}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func capturedClaudeSubagentIDs(
	sources []TranscriptSource, rootID string,
) []string {
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		if source.SessionID == "" || source.SessionID == rootID ||
			!strings.Contains(source.RawSource.Path, "/subagents/") {
			continue
		}
		seen[source.SessionID] = struct{}{}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func validateClaudeSubagentReferences(
	ctx context.Context,
	database *db.DB,
	rootID string,
	descendants []db.Session,
	capturedIDs []string,
) error {
	captured := make(map[string]struct{}, len(capturedIDs))
	for _, id := range capturedIDs {
		captured[id] = struct{}{}
	}
	sessionIDs := make([]string, 0, len(descendants)+1)
	sessionIDs = append(sessionIDs, rootID)
	for _, descendant := range descendants {
		sessionIDs = append(sessionIDs, descendant.ID)
	}
	for _, sessionID := range sessionIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		messages, err := database.GetAllMessages(ctx, sessionID)
		if err != nil {
			return err
		}
		for _, message := range messages {
			for _, call := range message.ToolCalls {
				childID := call.SubagentSessionID
				if childID == "" || childID == sessionID {
					continue
				}
				if _, ok := captured[childID]; !ok {
					return errorWithReason(
						ReasonSourceUnavailable,
						fmt.Sprintf(
							"exact Claude child source %q is unavailable", childID),
					)
				}
			}
		}
	}
	return nil
}

func (c *ingestedCapture) close(ctx context.Context) error {
	if c == nil || c.Database == nil {
		return nil
	}
	err := c.Database.CloseContext(ctx)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(err, ctxErr)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Join(err, ctxErr)
		}
		path := c.Database.Path() + suffix
		if chmodErr := os.Chmod(path, 0o600); chmodErr != nil &&
			!errors.Is(chmodErr, os.ErrNotExist) && err == nil {
			err = chmodErr
		}
	}
	return err
}
