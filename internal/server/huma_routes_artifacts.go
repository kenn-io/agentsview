package server

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"time"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/db"
)

func (s *Server) registerArtifactRoutes() {
	group := newRouteGroup(s.api, "/api/v1/artifacts", "Artifacts")

	get(s, group, "/origins", "List artifact origins", s.humaListArtifactOrigins)
	get(s, group, "/peers", "List artifact peers", s.humaListArtifactPeers)
	get(s, group, "/{origin}/index", "List artifact index for an origin", s.humaGetArtifactIndex)
	raw(s, group, http.MethodGet, "/{origin}/checkpoint", "Get latest artifact checkpoint", s.humaGetArtifactCheckpoint)
	raw(s, group, http.MethodGet, "/{origin}/{kind}/{name}", "Get artifact", s.humaGetArtifact)
	post(s, group, "/{origin}/{kind}/{name}", "Post artifact", s.humaPostArtifact)
}

type artifactOriginInput struct {
	Origin string `path:"origin" required:"true" doc:"Artifact origin ID"`
}

type artifactPathInput struct {
	Origin string `path:"origin" required:"true" doc:"Artifact origin ID"`
	Kind   string `path:"kind" required:"true" doc:"Artifact kind"`
	Name   string `path:"name" required:"true" doc:"Artifact filename or hash"`
}

type artifactPostInput struct {
	Origin  string `path:"origin" required:"true" doc:"Artifact origin ID"`
	Kind    string `path:"kind" required:"true" doc:"Artifact kind"`
	Name    string `path:"name" required:"true" doc:"Artifact filename or hash"`
	RawBody []byte `contentType:"application/octet-stream"`
}

type artifactOriginsResponse struct {
	Origins []string `json:"origins"`
}

// artifactPeer is one origin's status in the peers view: what it has published
// (from its latest checkpoint) and how much of it has landed locally.
type artifactPeer struct {
	Origin            string `json:"origin"`
	IsLocal           bool   `json:"is_local"`
	CheckpointSeq     int    `json:"checkpoint_seq"`
	PublishedSessions int    `json:"published_sessions"`
	LocalSessions     int    `json:"local_sessions"`
	LastPublished     string `json:"last_published,omitempty"`
}

type artifactPeersResponse struct {
	LocalOrigin   string         `json:"local_origin"`
	Peers         []artifactPeer `json:"peers"`
	ConflictCount int            `json:"conflict_count"`
}

type artifactPostResponse struct {
	Origin    string `json:"origin"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Hash      string `json:"hash,omitempty"`
	Size      int64  `json:"size"`
	Duplicate bool   `json:"duplicate"`
}

func (s *Server) artifactRoot() (string, error) {
	if s.cfg.DataDir == "" {
		return "", apiError(http.StatusServiceUnavailable, "artifact store not configured")
	}
	return filepath.Join(s.cfg.DataDir, "artifacts"), nil
}

func (s *Server) humaListArtifactOrigins(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[artifactOriginsResponse], error) {
	root, err := s.artifactRoot()
	if err != nil {
		return nil, err
	}
	origins, err := artifact.ListOrigins(root)
	if err != nil {
		return nil, artifactRouteError("list artifact origins", err)
	}
	return &jsonOutput[artifactOriginsResponse]{
		Body: artifactOriginsResponse{Origins: origins},
	}, nil
}

func (s *Server) humaGetArtifactIndex(
	_ context.Context,
	in *artifactOriginInput,
) (*jsonOutput[artifact.OriginArtifactIndex], error) {
	root, err := s.artifactRoot()
	if err != nil {
		return nil, err
	}
	index, err := artifact.ListArtifacts(root, in.Origin)
	if err != nil {
		return nil, artifactRouteError("list artifact index", err)
	}
	return &jsonOutput[artifact.OriginArtifactIndex]{Body: index}, nil
}

// localArtifactOrigin returns this machine's artifact origin without creating
// one. It prefers the configured origin and falls back to the persisted DB
// value so read-only callers never mint a new identity.
func (s *Server) localArtifactOrigin() string {
	if s.cfg.ArtifactOriginID != "" {
		return s.cfg.ArtifactOriginID
	}
	if local, ok := s.db.(*db.DB); ok {
		if origin, err := artifact.StoredOrigin(local); err == nil {
			return origin
		}
	}
	return ""
}

func (s *Server) humaListArtifactPeers(
	ctx context.Context,
	_ *emptyInput,
) (*jsonOutput[artifactPeersResponse], error) {
	root, err := s.artifactRoot()
	if err != nil {
		return nil, err
	}
	origins, err := artifact.ListOrigins(root)
	if err != nil {
		return nil, artifactRouteError("list artifact origins", err)
	}
	counts, err := s.db.MachineSessionCounts(ctx)
	if err != nil {
		return nil, internalError("machine session counts", err)
	}
	conflicts, err := s.db.CountMetadataConflicts(ctx)
	if err != nil {
		return nil, internalError("count metadata conflicts", err)
	}

	localOrigin := s.localArtifactOrigin()
	// Always surface this machine even before its first export.
	seen := map[string]bool{}
	ordered := make([]string, 0, len(origins)+1)
	if localOrigin != "" {
		ordered = append(ordered, localOrigin)
		seen[localOrigin] = true
	}
	for _, origin := range origins {
		if seen[origin] {
			continue
		}
		seen[origin] = true
		ordered = append(ordered, origin)
	}

	peers := make([]artifactPeer, 0, len(ordered))
	for _, origin := range ordered {
		summary, err := artifact.CheckpointSummary(root, origin)
		if err != nil {
			return nil, artifactRouteError("read artifact checkpoint", err)
		}
		isLocal := origin == localOrigin
		machineKey := origin
		if isLocal {
			// Owned sessions keep machine "local" in the local DB.
			machineKey = "local"
		}
		last := ""
		if summary.Found {
			last = summary.ModTime.UTC().Format(time.RFC3339)
		}
		peers = append(peers, artifactPeer{
			Origin:            origin,
			IsLocal:           isLocal,
			CheckpointSeq:     summary.Sequence,
			PublishedSessions: summary.SessionCount,
			LocalSessions:     counts[machineKey],
			LastPublished:     last,
		})
	}

	return &jsonOutput[artifactPeersResponse]{
		Body: artifactPeersResponse{
			LocalOrigin:   localOrigin,
			Peers:         peers,
			ConflictCount: conflicts,
		},
	}, nil
}

func (s *Server) humaGetArtifactCheckpoint(
	_ context.Context,
	in *artifactOriginInput,
) (*bytesOutput, error) {
	root, err := s.artifactRoot()
	if err != nil {
		return nil, err
	}
	art, err := artifact.ReadLatestCheckpoint(root, in.Origin)
	if err != nil {
		return nil, artifactRouteError("get artifact checkpoint", err)
	}
	return artifactBytesOutput(art), nil
}

func (s *Server) humaGetArtifact(
	_ context.Context,
	in *artifactPathInput,
) (*bytesOutput, error) {
	root, err := s.artifactRoot()
	if err != nil {
		return nil, err
	}
	art, err := artifact.ReadArtifact(root, in.Origin, in.Kind, in.Name)
	if err != nil {
		return nil, artifactRouteError("get artifact", err)
	}
	return artifactBytesOutput(art), nil
}

func (s *Server) humaPostArtifact(
	ctx context.Context,
	in *artifactPostInput,
) (*jsonOutput[artifactPostResponse], error) {
	root, err := s.artifactRoot()
	if err != nil {
		return nil, err
	}
	res, err := artifact.WriteArtifact(root, in.Origin, in.Kind, in.Name, in.RawBody)
	if err != nil {
		return nil, artifactRouteError("post artifact", err)
	}
	if err := s.importPeerArtifacts(ctx, root); err != nil {
		return nil, err
	}
	return &jsonOutput[artifactPostResponse]{
		Body: artifactPostResponse{
			Origin:    res.Origin,
			Kind:      res.Kind,
			Name:      res.Name,
			Hash:      res.Hash,
			Size:      res.Size,
			Duplicate: res.Duplicate,
		},
	}, nil
}

func (s *Server) importPeerArtifacts(ctx context.Context, root string) error {
	local, ok := s.db.(*db.DB)
	if !ok {
		return nil
	}
	var res artifact.ImportResult
	var err error
	if s.metadata != nil {
		// Route through the recorder so import and local appends share one HLC
		// clock, keeping later local edits causally ahead of imported peers.
		res, err = s.metadata.Import(ctx, root)
	} else {
		localOrigin := s.cfg.ArtifactOriginID
		if localOrigin == "" {
			localOrigin, err = artifact.EnsureOrigin(local)
			if err != nil {
				return internalError("artifact import origin", err)
			}
		}
		res, err = artifact.ImportDetailed(ctx, local, root, localOrigin)
	}
	if err != nil {
		return artifactRouteError("import peer artifacts", err)
	}
	if res.Changed() && s.broadcaster != nil {
		s.broadcaster.Emit("messages")
	}
	return nil
}

func artifactBytesOutput(art artifact.PeerArtifact) *bytesOutput {
	return &bytesOutput{
		ContentType:  art.ContentType,
		NoSniff:      "nosniff",
		CacheControl: "no-store",
		Body:         art.Data,
	}
}

func artifactRouteError(logPrefix string, err error) error {
	switch {
	case errors.Is(err, artifact.ErrArtifactInvalid):
		return apiError(http.StatusBadRequest, err.Error())
	case errors.Is(err, artifact.ErrArtifactNotFound):
		return apiError(http.StatusNotFound, "artifact not found")
	case errors.Is(err, artifact.ErrArtifactConflict):
		return apiError(http.StatusConflict, "artifact conflict")
	default:
		return internalError(logPrefix, err)
	}
}
