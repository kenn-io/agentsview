package parser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func newDevinProviderFactory(def AgentDef) ProviderFactory {
	return devinProviderFactory{def: cloneAgentDef(def)}
}

type devinProviderFactory struct {
	def AgentDef
}

func (f devinProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f devinProviderFactory) Capabilities() Capabilities {
	return devinProviderCapabilities()
}

func (f devinProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &devinProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   devinProviderCapabilities(),
			Config: cfg,
		},
		sources: newDevinSourceSet(cfg.Roots),
	}
}

type devinProvider struct {
	ProviderBase
	sources devinSourceSet
}

func (p *devinProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *devinProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *devinProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *devinProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *devinProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *devinProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	src, ok := p.sources.sourceFromRef(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("%s source path unavailable", AgentDevin)
	}
	if _, err := os.Stat(src.DBPath); err != nil {
		if os.IsNotExist(err) {
			return ParseOutcome{
				ResultSetComplete: true,
				SkipReason:        SkipNoSession,
			}, nil
		}
		return ParseOutcome{}, fmt.Errorf("stat %s: %w", src.DBPath, err)
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	sess, msgs, err := parseDevinSession(src.DBPath, src.SessionID, machine)
	if errors.Is(err, sql.ErrNoRows) {
		return ParseOutcome{
			ResultSetComplete: true,
			ForceReplace:      true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	var transcriptErr *devinTranscriptError
	if errors.As(err, &transcriptErr) {
		return ParseOutcome{}, transcriptErr
	}
	if err != nil {
		return ParseOutcome{}, err
	}
	if sess == nil {
		return ParseOutcome{
			ResultSetComplete: true,
			ForceReplace:      true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	if req.Fingerprint.Hash != "" {
		sess.File.Hash = req.Fingerprint.Hash
	}
	return ParseOutcome{
		Results: []ParseResultOutcome{{
			Result:      ParseResult{Session: *sess, Messages: msgs},
			DataVersion: DataVersionCurrent,
		}},
		ResultSetComplete: true,
		ForceReplace:      true,
	}, nil
}

type devinSource struct {
	Root      string
	DBPath    string
	SessionID string
}

type devinSourceSet struct {
	roots []string
}

func newDevinSourceSet(roots []string) devinSourceSet {
	return devinSourceSet{roots: cleanJSONLRoots(roots)}
}

func (s devinSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dbPath := devinDBPath(root)
		if dbPath == "" {
			continue
		}
		metas, err := ListDevinSessionMeta(dbPath)
		if err != nil {
			return nil, err
		}
		for _, meta := range metas {
			addJSONLSource(s.newSourceRefWithMTime(root, dbPath, meta.RawSessionID, meta.FileMtime), &sources, seen)
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s devinSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots)*2)
	for _, root := range s.roots {
		cliRoot := filepath.Join(root, "cli")
		roots = append(roots,
			WatchRoot{
				Path:         cliRoot,
				Recursive:    false,
				IncludeGlobs: []string{devinDBFilename, devinDBFilename + "-*"},
				DebounceKey:  string(AgentDevin) + ":db:" + root,
			},
			WatchRoot{
				Path:         filepath.Join(cliRoot, "transcripts"),
				Recursive:    false,
				IncludeGlobs: []string{"*.json"},
				DebounceKey:  string(AgentDevin) + ":transcripts:" + root,
			},
		)
	}
	return WatchPlan{Roots: roots}, nil
}

func (s devinSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, root := range s.roots {
		if req.WatchRoot != "" && !samePath(req.WatchRoot, filepath.Join(root, "cli")) && !samePath(req.WatchRoot, filepath.Join(root, "cli", "transcripts")) {
			continue
		}
		if ref, ok := s.sourceRef(root, req.Path, true); ok {
			return []SourceRef{ref}, nil
		}
		if dbPath, ok := s.dbPathForEvent(root, req.Path); ok {
			metas, err := ListDevinSessionMeta(dbPath)
			if err != nil {
				return nil, err
			}
			sources := make([]SourceRef, 0, len(metas))
			seen := make(map[string]struct{}, len(metas))
			for _, meta := range metas {
				addJSONLSource(s.newSourceRefWithMTime(root, dbPath, meta.RawSessionID, meta.FileMtime), &sources, seen)
			}
			for _, path := range req.StoredSourcePaths {
				ref, ok := s.sourceRef(root, path, true)
				if !ok {
					continue
				}
				src := ref.Opaque.(devinSource)
				if !samePath(src.DBPath, dbPath) {
					continue
				}
				addJSONLSource(ref, &sources, seen)
			}
			sortJSONLSources(sources)
			return sources, nil
		}
		if sessionID, ok := s.transcriptSessionIDForEvent(root, req.Path); ok {
			if ref, ok, err := s.findByRawSessionID(root, sessionID, false); err != nil {
				return nil, err
			} else if ok {
				return []SourceRef{ref}, nil
			}
			for _, path := range req.StoredSourcePaths {
				ref, ok := s.sourceRef(root, path, true)
				if !ok {
					continue
				}
				src := ref.Opaque.(devinSource)
				if src.SessionID == sessionID {
					return []SourceRef{ref}, nil
				}
			}
		}
	}
	return nil, nil
}

func (s devinSourceSet) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	for _, path := range []string{req.StoredFilePath, req.FingerprintKey} {
		if path == "" {
			continue
		}
		for _, root := range s.roots {
			ref, ok := s.sourceRef(root, path, true)
			if !ok {
				continue
			}
			src := ref.Opaque.(devinSource)
			if req.RawSessionID != "" && src.SessionID != req.RawSessionID {
				continue
			}
			if req.RequireFreshSource {
				fresh, err := s.sourceExists(src)
				if err != nil {
					return SourceRef{}, false, err
				}
				if !fresh {
					continue
				}
			}
			return ref, true, nil
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		ref, ok, err := s.findByRawSessionID(root, req.RawSessionID, req.RequireFreshSource)
		if err != nil {
			return SourceRef{}, false, err
		}
		if ok {
			return ref, true, nil
		}
	}
	return SourceRef{}, false, nil
}

func (s devinSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	src, ok := s.sourceFromRef(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("%s source path unavailable", AgentDevin)
	}
	key := firstNonEmptyJSONLString(source.FingerprintKey, source.Key, src.virtualPath())
	dbInfo, err := os.Stat(src.DBPath)
	if err != nil {
		if os.IsNotExist(err) {
			return SourceFingerprint{Key: key}, nil
		}
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.DBPath, err)
	}
	meta, err := getDevinSessionMeta(src.DBPath, src.SessionID)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if meta == nil {
		return SourceFingerprint{Key: key}, nil
	}
	fingerprint := SourceFingerprint{Key: key}
	transcriptPath := filepath.Join(filepath.Dir(src.DBPath), "transcripts", src.SessionID+".json")
	transcriptInfo, err := os.Stat(transcriptPath)
	if err != nil && !os.IsNotExist(err) {
		return SourceFingerprint{}, newDevinTranscriptError("stat", err)
	}
	fingerprint.MTimeNS = maxDevinFingerprintMTime(meta, dbInfo, transcriptInfo)
	hash, err := devinFingerprintHash(meta, src.DBPath, transcriptPath, transcriptInfo)
	if err != nil {
		return SourceFingerprint{}, err
	}
	fingerprint.Hash = hash
	if transcriptInfo != nil {
		fingerprint.Size = transcriptInfo.Size()
	}
	return fingerprint, nil
}

func maxDevinFingerprintMTime(
	meta *DevinSessionMeta,
	dbInfo os.FileInfo,
	transcriptInfo os.FileInfo,
) int64 {
	var mtime int64
	if meta != nil {
		mtime = meta.FileMtime
		if created := meta.CreatedAt.UnixNano(); created > mtime {
			mtime = created
		}
		if lastActivity := meta.LastActivity.UnixNano(); lastActivity > mtime {
			mtime = lastActivity
		}
	}
	if dbInfo != nil {
		if dbMTime := dbInfo.ModTime().UnixNano(); dbMTime > mtime {
			mtime = dbMTime
		}
	}
	if transcriptInfo != nil {
		if transcriptMTime := transcriptInfo.ModTime().UnixNano(); transcriptMTime > mtime {
			mtime = transcriptMTime
		}
	}
	return mtime
}

func devinFingerprintHash(
	meta *DevinSessionMeta,
	dbPath string,
	transcriptPath string,
	transcriptInfo os.FileInfo,
) (string, error) {
	h := sha256.New()
	if _, err := fmt.Fprintf(
		h,
		"session\x00%s\x00created\x00%d\x00last_activity\x00%d\x00updated\x00%d\x00title\x00%s\x00cwd\x00%s\x00model\x00%s\x00",
		meta.RawSessionID,
		meta.CreatedAt.UnixMilli(),
		meta.LastActivity.UnixMilli(),
		meta.UpdatedAt.UnixMilli(),
		meta.Title,
		meta.CWD,
		meta.Model,
	); err != nil {
		return "", err
	}
	if transcriptInfo == nil {
		if err := devinAppendMessageNodesFingerprint(h, dbPath, meta.RawSessionID); err != nil {
			return "", err
		}
		return fmt.Sprintf("%x", h.Sum(nil)), nil
	}
	if _, err := fmt.Fprintf(
		h,
		"transcript_size\x00%d\x00transcript_mtime\x00%d\x00",
		transcriptInfo.Size(),
		transcriptInfo.ModTime().UnixNano(),
	); err != nil {
		return "", err
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return "", newDevinTranscriptError("open", err)
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return "", newDevinTranscriptError("hash", err)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func devinAppendMessageNodesFingerprint(h io.Writer, dbPath, rawSessionID string) error {
	nodes, err := listDevinMessageNodes(dbPath, rawSessionID)
	if err != nil {
		return err
	}
	var maxCreatedAtMS int64
	for _, node := range nodes {
		if node.CreatedAtMS > maxCreatedAtMS {
			maxCreatedAtMS = node.CreatedAtMS
		}
	}
	if _, err := fmt.Fprintf(h, "message_nodes\x00count\x00%d\x00max_created\x00%d\x00", len(nodes), maxCreatedAtMS); err != nil {
		return err
	}
	for _, node := range nodes {
		if _, err := fmt.Fprintf(
			h,
			"row\x00%d\x00node\x00%d\x00parent_valid\x00%t\x00parent\x00%d\x00created\x00%d\x00chat_len\x00%d\x00",
			node.RowID,
			node.NodeID,
			node.ParentNodeID.Valid,
			node.ParentNodeID.Int64,
			node.CreatedAtMS,
			len(node.ChatMessage),
		); err != nil {
			return err
		}
		if _, err := io.WriteString(h, node.ChatMessage); err != nil {
			return err
		}
		if _, err := io.WriteString(h, "\x00"); err != nil {
			return err
		}
	}
	return nil
}

func (s devinSourceSet) sourceFromRef(source SourceRef) (devinSource, bool) {
	switch src := source.Opaque.(type) {
	case devinSource:
		return src, src.DBPath != "" && src.SessionID != ""
	case *devinSource:
		if src != nil && src.DBPath != "" && src.SessionID != "" {
			return *src, true
		}
	}
	for _, candidate := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		for _, root := range s.roots {
			if ref, ok := s.sourceRef(root, candidate, true); ok {
				return ref.Opaque.(devinSource), true
			}
		}
	}
	return devinSource{}, false
}

func (s devinSourceSet) sourceRef(root, path string, allowMissing bool) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	dbPath, sessionID, ok := ParseVirtualSourcePathForBase(path, devinDBFilename)
	if !ok || sessionID == "" {
		return SourceRef{}, false
	}
	if !samePath(dbPath, filepath.Join(root, "cli", devinDBFilename)) {
		return SourceRef{}, false
	}
	if !allowMissing && !IsRegularFile(dbPath) {
		return SourceRef{}, false
	}
	return s.newSourceRef(root, dbPath, sessionID), true
}

func (s devinSourceSet) dbPathForEvent(root, path string) (string, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	cliRoot := filepath.Join(root, "cli")
	rel, ok := relUnder(cliRoot, path)
	if !ok || strings.Contains(rel, string(filepath.Separator)) {
		return "", false
	}
	if rel == devinDBFilename || rel == devinDBFilename+"-wal" || rel == devinDBFilename+"-shm" {
		return filepath.Join(cliRoot, devinDBFilename), true
	}
	return "", false
}

func (s devinSourceSet) transcriptSessionIDForEvent(root, path string) (string, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, ok := relUnder(filepath.Join(root, "cli", "transcripts"), path)
	if !ok || strings.Contains(rel, string(filepath.Separator)) || filepath.Ext(rel) != ".json" {
		return "", false
	}
	return strings.TrimSuffix(rel, ".json"), true
}

func (s devinSourceSet) sourceExists(src devinSource) (bool, error) {
	if !IsRegularFile(src.DBPath) {
		return false, nil
	}
	meta, err := getDevinSessionMeta(src.DBPath, src.SessionID)
	if err != nil {
		return false, err
	}
	return meta != nil, nil
}

func (s devinSourceSet) findByRawSessionID(root, rawID string, requireFresh bool) (SourceRef, bool, error) {
	if root == "" || rawID == "" {
		return SourceRef{}, false, nil
	}
	dbPath := devinDBPath(root)
	if dbPath == "" {
		return SourceRef{}, false, nil
	}
	meta, err := getDevinSessionMeta(dbPath, rawID)
	if err != nil {
		return SourceRef{}, false, err
	}
	if meta == nil {
		return SourceRef{}, false, nil
	}
	ref := s.newSourceRefWithMTime(root, dbPath, rawID, meta.FileMtime)
	if requireFresh {
		fresh, err := s.sourceExists(ref.Opaque.(devinSource))
		if err != nil {
			return SourceRef{}, false, err
		}
		if !fresh {
			return SourceRef{}, false, nil
		}
	}
	return ref, true, nil
}

func (s devinSourceSet) newSourceRef(root, dbPath, sessionID string) SourceRef {
	return s.newSourceRefWithMTime(root, dbPath, sessionID, 0)
}

func (s devinSourceSet) newSourceRefWithMTime(root, dbPath, sessionID string, mtimeNS int64) SourceRef {
	virtualPath := VirtualSourcePath(dbPath, sessionID)
	return SourceRef{
		Provider:         AgentDevin,
		Key:              virtualPath,
		DisplayPath:      virtualPath,
		FingerprintKey:   virtualPath,
		DiscoveryMTimeNS: mtimeNS,
		Opaque: devinSource{
			Root:      root,
			DBPath:    dbPath,
			SessionID: sessionID,
		},
	}
}

func (s devinSource) virtualPath() string {
	return VirtualSourcePath(s.DBPath, s.SessionID)
}

func devinProviderCapabilities() Capabilities {
	return Capabilities{
		Source: dbBackedSourceCapabilities(CapabilityNotApplicable),
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
