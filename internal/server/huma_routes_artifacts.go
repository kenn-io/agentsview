package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/db"
)

func (s *Server) registerArtifactRoutes() {
	if s.artifactStore == nil && !s.documentArtifactRoutes {
		return
	}
	group := newRouteGroup(s.api, "/api/v1/artifacts", "Artifacts")

	get(s, group, "/origins", "List artifact origins", s.humaListArtifactOrigins)
	get(s, group, "/peers", "List artifact peers", s.humaListArtifactPeers)
	get(s, group, "/{origin}/index", "List artifact index for an origin", s.humaGetArtifactIndex)
	writable := s.documentArtifactRoutes || s.hasWritableArtifactStore()
	if writable {
		post(s, group, "/finalize", "Finalize artifact uploads", s.humaFinalizeArtifacts)
	}
	s.documentArtifactStreamingRoutes(writable)
	s.mux.HandleFunc("GET /api/v1/artifacts/{origin}/checkpoint", s.getArtifactCheckpointHTTP)
	s.mux.HandleFunc("GET /api/v1/artifacts/{origin}/{kind}/{name}", s.getArtifactHTTP)
	if writable {
		s.mux.HandleFunc("POST /api/v1/artifacts/{origin}/{kind}/{name}", s.postArtifactHTTP)
		s.mux.HandleFunc("POST /api/v1/artifacts/exchange", s.postArtifactExchangeHTTP)
		s.mux.HandleFunc("POST /api/v1/artifacts/maintenance", s.postArtifactMaintenanceHTTP)
		s.mux.HandleFunc("POST /api/v1/artifacts/reset", s.postArtifactResetHTTP)
	}
	s.mux.HandleFunc("DELETE /api/v1/artifacts/cursors/{cursor}", s.releaseArtifactCursorHTTP)
}

func (s *Server) documentArtifactStreamingRoutes(documentPost bool) {
	binary := &huma.Schema{Type: huma.TypeString, Format: "binary"}
	stringParam := func(name, description string) *huma.Param {
		return &huma.Param{
			Name: name, In: "path", Description: description, Required: true,
			Schema: &huma.Schema{Type: huma.TypeString},
		}
	}
	originParam := func() *huma.Param { return stringParam("origin", "Artifact origin ID") }

	s.api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: operationID(http.MethodGet, "/api/v1/artifacts/{origin}/checkpoint"),
		Method:      http.MethodGet,
		Path:        "/api/v1/artifacts/{origin}/checkpoint",
		Tags:        []string{"Artifacts"},
		Summary:     "Get latest artifact checkpoint",
		Parameters:  []*huma.Param{originParam()},
		Responses: artifactStreamingResponses(&huma.Response{
			Description: "OK",
			Content: map[string]*huma.MediaType{
				"application/octet-stream": {Schema: binary},
			},
		}),
	})

	path := "/api/v1/artifacts/{origin}/{kind}/{name}"
	params := func() []*huma.Param {
		return []*huma.Param{
			originParam(),
			stringParam("kind", "Artifact kind"),
			stringParam("name", "Artifact filename or hash"),
		}
	}
	s.api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: operationID(http.MethodGet, path),
		Method:      http.MethodGet,
		Path:        path,
		Tags:        []string{"Artifacts"},
		Summary:     "Get artifact",
		Parameters:  params(),
		Responses: artifactStreamingResponses(&huma.Response{
			Description: "OK",
			Content: map[string]*huma.MediaType{
				"application/octet-stream": {Schema: binary},
			},
		}),
	})
	if !documentPost {
		return
	}
	postSchema := s.api.OpenAPI().Components.Schemas.Schema(
		reflect.TypeFor[artifactPostResponse](), true, "ArtifactPostResponse",
	)
	s.api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: operationID(http.MethodPost, path),
		Method:      http.MethodPost,
		Path:        path,
		Tags:        []string{"Artifacts"},
		Summary:     "Post artifact",
		Parameters:  params(),
		RequestBody: &huma.RequestBody{
			Required: true,
			Content: map[string]*huma.MediaType{
				"application/octet-stream": {Schema: binary},
			},
		},
		Responses: artifactStreamingResponses(&huma.Response{
			Description: "OK",
			Content: map[string]*huma.MediaType{
				"application/json": {Schema: postSchema},
			},
		}),
	})
}

func artifactStreamingResponses(success *huma.Response) map[string]*huma.Response {
	responses := map[string]*huma.Response{"200": success}
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusConflict,
		http.StatusInternalServerError,
		http.StatusNotImplemented,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		responses[strconv.Itoa(status)] = &huma.Response{
			Description: http.StatusText(status),
			Content: map[string]*huma.MediaType{
				"text/plain": {Schema: &huma.Schema{Type: huma.TypeString}},
			},
		}
	}
	return responses
}

type artifactOriginsInput struct {
	Cursor string `query:"cursor" doc:"Opaque artifact origin cursor"`
	Limit  int    `query:"limit" minimum:"1" maximum:"512" default:"512" doc:"Maximum origins to return"`
}

type artifactIndexInput struct {
	Origin string `path:"origin" required:"true" doc:"Artifact origin ID"`
	Cursor string `query:"cursor" doc:"Opaque artifact index cursor"`
	Limit  int    `query:"limit" minimum:"1" maximum:"512" default:"512" doc:"Maximum artifact names to return"`
}

type artifactIndexResponse struct {
	artifact.OriginArtifactIndex
	NextCursor string `json:"next_cursor,omitempty"`
}

type artifactPostInput struct {
	Origin     string
	Kind       string
	Name       string
	ImportMode string
	Body       io.Reader
}

type artifactFinalizeResponse struct {
	ImportedSessions int `json:"imported_sessions"`
	ImportedMessages int `json:"imported_messages"`
	ImportedMetadata int `json:"imported_metadata"`
	Deferred         int `json:"deferred"`
}

type artifactOriginsResponse struct {
	Origins    []string `json:"origins"`
	NextCursor string   `json:"next_cursor,omitempty"`
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
	Status            string `json:"status"`
}

type artifactPeersResponse struct {
	LocalOrigin     string         `json:"local_origin"`
	Peers           []artifactPeer `json:"peers"`
	ConflictCount   int            `json:"conflict_count"`
	PendingImports  int            `json:"pending_imports"`
	OldestPendingAt string         `json:"oldest_pending_at,omitempty"`
	NextCursor      string         `json:"next_cursor,omitempty"`
}

type artifactPeersInput struct {
	Cursor string `query:"cursor" doc:"Opaque peer origin cursor"`
	Limit  int    `query:"limit" minimum:"1" maximum:"512" default:"512" doc:"Maximum peer origins to return"`
}

type artifactPostResponse struct {
	Origin    string `json:"origin"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Hash      string `json:"hash,omitempty"`
	Size      int64  `json:"size"`
	Duplicate bool   `json:"duplicate"`
}

type artifactExchangeRequest struct {
	Target           string `json:"target"`
	Token            string `json:"token,omitempty"`
	AllowInsecure    bool   `json:"allow_insecure,omitempty"`
	BaselineMetadata bool   `json:"baseline_metadata,omitempty"`
}

type artifactExchangeResponse struct {
	Origin           string `json:"origin"`
	ExportedSessions int    `json:"exported_sessions"`
	ImportedSessions int    `json:"imported_sessions"`
	ImportedMessages int    `json:"imported_messages"`
	ImportedMetadata int    `json:"imported_metadata"`
}

type artifactMaintenanceRequest struct {
	Grace                  string `json:"grace,omitempty"`
	QuarantineGrace        string `json:"quarantine_grace,omitempty"`
	GraceSeconds           *int64 `json:"grace_seconds,omitempty"`
	QuarantineGraceSeconds *int64 `json:"quarantine_grace_seconds,omitempty"`
	MaxObjects             int    `json:"max_objects"`
	MaxBytes               int64  `json:"max_bytes"`
	DryRun                 bool   `json:"dry_run"`
	TrashCursor            string `json:"trash_cursor,omitempty"`
	GCCursor               string `json:"gc_cursor,omitempty"`
	RepackCursor           string `json:"repack_cursor,omitempty"`
}

type artifactResetResponse struct {
	artifact.RepositoryResetResult
	ManualCleanup    string `json:"manual_cleanup"`
	ForeignArtifacts string `json:"foreign_artifacts"`
}

const maxArtifactMaintenanceGraceSeconds = int64(1<<63-1) / int64(time.Second)

func parseArtifactMaintenanceDuration(exact string, legacySeconds *int64) (time.Duration, error) {
	if exact != "" && legacySeconds != nil {
		return 0, errors.New("duration and legacy seconds cannot both be set")
	}
	if exact != "" {
		duration, err := time.ParseDuration(exact)
		if err != nil || duration < 0 {
			return 0, errors.New("invalid duration")
		}
		return duration, nil
	}
	if legacySeconds == nil {
		return 0, nil
	}
	if *legacySeconds < 0 || *legacySeconds > maxArtifactMaintenanceGraceSeconds {
		return 0, errors.New("invalid legacy duration")
	}
	return time.Duration(*legacySeconds) * time.Second, nil
}

type artifactMaintenancePhysicalResponse struct {
	Supported bool                               `json:"supported"`
	Result    artifact.PhysicalMaintenanceResult `json:"result"`
}

type artifactMaintenanceResponse struct {
	Logical  artifact.GCResult                   `json:"logical"`
	Physical artifactMaintenancePhysicalResponse `json:"physical"`
}

func (s *Server) acquireArtifactStore() (artifact.ArtifactStore, func(), error) {
	store, release, err := s.artifactOps.acquire()
	if err != nil {
		return nil, nil, apiError(http.StatusServiceUnavailable, "artifact store not configured")
	}
	return store, release, nil
}

func (s *Server) humaListArtifactOrigins(
	ctx context.Context,
	in *artifactOriginsInput,
) (*jsonOutput[artifactOriginsResponse], error) {
	store, release, err := s.acquireArtifactStore()
	if err != nil {
		return nil, err
	}
	defer release()
	if err := s.publishLocalArtifacts(ctx, store); err != nil {
		return nil, err
	}
	limit := in.Limit
	if limit == 0 {
		limit = 512
	}
	origins, next, err := s.currentArtifactStoreCursorRegistry().originPage(
		ctx, store, in.Cursor, limit,
	)
	if err != nil {
		return nil, artifactRouteError("list artifact origins", err)
	}
	return &jsonOutput[artifactOriginsResponse]{
		Body: artifactOriginsResponse{Origins: origins, NextCursor: next},
	}, nil
}

func (s *Server) humaGetArtifactIndex(
	ctx context.Context,
	in *artifactIndexInput,
) (*jsonOutput[artifactIndexResponse], error) {
	store, release, err := s.acquireArtifactStore()
	if err != nil {
		return nil, err
	}
	defer release()
	limit := in.Limit
	if limit == 0 {
		limit = 512
	}
	index, next, err := s.currentArtifactStoreCursorRegistry().indexPage(
		ctx, store, in.Origin, in.Cursor, limit,
	)
	if err != nil {
		return nil, artifactRouteError("list artifact index", err)
	}
	return &jsonOutput[artifactIndexResponse]{Body: artifactIndexResponse{
		OriginArtifactIndex: index,
		NextCursor:          next,
	}}, nil
}

func (s *Server) releaseArtifactCursorHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.Context().Err(); err != nil {
		return
	}
	_, release, err := s.acquireArtifactStore()
	if err != nil {
		writeArtifactHTTPError(w, err)
		return
	}
	defer release()
	s.artifactCursors.release(r.PathValue("cursor"))
	s.currentArtifactStoreCursorRegistry().release(r.PathValue("cursor"))
	w.WriteHeader(http.StatusNoContent)
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
	in *artifactPeersInput,
) (*jsonOutput[artifactPeersResponse], error) {
	store, release, err := s.acquireArtifactStore()
	if err != nil {
		return nil, err
	}
	defer release()
	if err := s.publishLocalArtifacts(ctx, store); err != nil {
		return nil, err
	}
	limit := in.Limit
	if limit == 0 {
		limit = 512
	}
	origins, next, err := s.currentArtifactStoreCursorRegistry().peerOriginPage(
		ctx, store, in.Cursor, limit,
	)
	if err != nil {
		return nil, artifactRouteError("list artifact origins", err)
	}
	conflicts, err := s.db.CountMetadataConflicts(ctx)
	if err != nil {
		return nil, internalError("count metadata conflicts", err)
	}

	localOrigin := s.localArtifactOrigin()
	peers := make([]artifactPeer, 0, len(origins))
	for _, origin := range origins {
		isLocal := origin == localOrigin
		sequence, expected, found, headErr := s.artifactPeerCheckpointHead(ctx, origin, isLocal)
		landing := artifact.OriginCheckpointLanding{}
		if found {
			landing.Sequence = sequence
		}
		peerStatus := "pending"
		if headErr != nil {
			peerStatus = "error"
		} else if found {
			landing, err = artifact.CheckpointLandingStatusAtStoreHead(
				ctx, store, origin, sequence, expected, s.db, isLocal,
			)
			if err != nil {
				landing.Sequence = sequence
			}
			switch {
			case err == nil && landing.LandedSessionCount >= landing.SessionCount:
				peerStatus = "in_sync"
			case err == nil, errors.Is(err, artifact.ErrArtifactNotFound):
				peerStatus = "pending"
			default:
				peerStatus = "error"
			}
		}
		last := ""
		if landing.Found {
			last = landing.ModTime.UTC().Format(time.RFC3339)
		}
		peers = append(peers, artifactPeer{
			Origin:            origin,
			IsLocal:           isLocal,
			CheckpointSeq:     landing.Sequence,
			PublishedSessions: landing.SessionCount,
			LocalSessions:     landing.LandedSessionCount,
			LastPublished:     last,
			Status:            peerStatus,
		})
	}

	pendingImports := 0
	oldestPendingAt := ""
	if local, ok := s.db.(*db.DB); ok {
		pendingImports, oldestPendingAt, err = local.ArtifactImportQueueStats(ctx)
		if err != nil {
			return nil, artifactRouteError("read artifact import queue", err)
		}
	}
	return &jsonOutput[artifactPeersResponse]{
		Body: artifactPeersResponse{
			LocalOrigin: localOrigin, Peers: peers, ConflictCount: conflicts,
			PendingImports: pendingImports, OldestPendingAt: oldestPendingAt,
			NextCursor: next,
		},
	}, nil
}

func (s *Server) artifactPeerCheckpointHead(
	ctx context.Context, origin string, isLocal bool,
) (int, artifact.Identity, bool, error) {
	database, ok := s.db.(interface {
		GetArtifactCheckpointHead(context.Context, string) (db.ArtifactCheckpointHead, bool, error)
		GetArtifactPeerCheckpointHead(context.Context, string) (db.ArtifactPeerCheckpointHead, bool, error)
		GetArtifactCheckpointLandingHead(context.Context, string) (db.ArtifactCheckpointLanding, bool, error)
	})
	if !ok {
		return 0, artifact.Identity{}, false, nil
	}
	if isLocal {
		head, found, err := database.GetArtifactCheckpointHead(ctx, origin)
		if err != nil || !found {
			return 0, artifact.Identity{}, found, err
		}
		identity, err := artifact.NewIdentity(head.CheckpointSHA256, head.CheckpointSize)
		return head.Sequence, identity, true, err
	}
	head, headFound, err := database.GetArtifactPeerCheckpointHead(ctx, origin)
	if err != nil {
		return 0, artifact.Identity{}, false, err
	}
	landing, landed, err := database.GetArtifactCheckpointLandingHead(ctx, origin)
	if err != nil {
		return 0, artifact.Identity{}, false, err
	}
	if landed && (!headFound || landing.Sequence > head.Sequence) {
		return landing.Sequence, artifact.Identity{}, true, nil
	}
	if !headFound {
		return 0, artifact.Identity{}, false, nil
	}
	identity, err := artifact.NewIdentity(head.CheckpointSHA256, head.CheckpointSize)
	return head.Sequence, identity, true, err
}

// publishLocalArtifacts refreshes the server's owned origin immediately before
// peer discovery. HTTP transports begin every exchange with origin discovery,
// so this makes the server a publisher without requiring a separate folder
// sync process while keeping individual artifact reads side-effect free.
func (s *Server) publishLocalArtifacts(ctx context.Context, store artifact.ArtifactStore) error {
	local, ok := s.db.(*db.DB)
	if !ok || local.ReadOnly() {
		return nil
	}
	origin := s.localArtifactOrigin()
	if origin == "" {
		return nil
	}

	s.lockSessionLifecycle()
	defer s.sessionLifecycleMu.Unlock()
	if s.artifactRepository != nil {
		_, recovered, err := artifact.RecoverRepositoryResetRepublish(
			ctx, local, s.artifactRepository, origin,
		)
		if err != nil {
			return artifactRouteError("recover artifact repository reset", err)
		}
		if recovered {
			s.artifactBaselineDone = true
		}
	}
	if !s.artifactBaselineDone && s.metadata != nil {
		if _, err := s.metadata.AppendBaseline(ctx); err != nil {
			return artifactRouteError("baseline local artifact metadata", err)
		}
		s.artifactBaselineDone = true
	}
	if s.engine != nil {
		s.engine.FlushSignals()
	}
	var err error
	if s.artifactRepository != nil {
		_, err = artifact.PublishRepositoryArtifacts(
			ctx, local, s.artifactRepository, artifact.ExportOptions{Origin: origin},
		)
	} else {
		_, err = artifact.ExportToStore(
			ctx, local, store, artifact.ExportOptions{Origin: origin},
		)
	}
	if err != nil {
		return artifactRouteError("export local artifacts", err)
	}
	if s.artifactRepository != nil {
		s.artifactRepository.NotifyBatch(ctx)
	}
	return nil
}

func (s *Server) getArtifactHTTP(w http.ResponseWriter, r *http.Request) {
	store, release, err := s.acquireArtifactStore()
	if err != nil {
		writeArtifactHTTPError(w, err)
		return
	}
	defer release()
	spool, err := spoolStoreArtifactForServe(
		r.Context(), store, r.PathValue("origin"), r.PathValue("kind"), r.PathValue("name"),
	)
	s.serveArtifactSpool(w, r, spool, err)
}

func (s *Server) getArtifactCheckpointHTTP(w http.ResponseWriter, r *http.Request) {
	store, release, err := s.acquireArtifactStore()
	if err != nil {
		writeArtifactHTTPError(w, err)
		return
	}
	defer release()
	spool, err := spoolLatestStoreCheckpointForServe(
		r.Context(), store, r.PathValue("origin"),
	)
	s.serveArtifactSpool(w, r, spool, err)
}

func (s *Server) serveArtifactSpool(
	w http.ResponseWriter, r *http.Request, spool *artifact.PeerArtifactSpool, err error,
) {
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		writeArtifactHTTPError(w, artifactRouteError("get artifact", err))
		return
	}
	if spool == nil {
		writeArtifactHTTPError(w, internalError(
			"get artifact", errors.New("artifact response spool is nil"),
		))
		return
	}
	defer func() {
		if err := spool.Close(); err != nil {
			log.Printf("closing artifact response spool: %v", err)
		}
	}()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(spool.Size, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, &artifactHTTPContextReader{
		ctx: r.Context(), reader: spool.File,
	}); err != nil && r.Context().Err() == nil {
		log.Printf("streaming artifact response: %v", err)
	}
}

type artifactHTTPContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *artifactHTTPContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

var artifactPeerKinds = [...]artifact.Kind{
	artifact.KindSegments,
	artifact.KindRaw,
	artifact.KindManifests,
	artifact.KindMeta,
	artifact.KindCheckpoints,
}

func latestStoreCheckpointEntry(
	ctx context.Context, store artifact.ArtifactStore, origin string,
) (_ artifact.Entry, found bool, retErr error) {
	iterator, err := store.Entries(ctx, origin, artifact.KindCheckpoints)
	if err != nil {
		return artifact.Entry{}, false, err
	}
	defer func() { retErr = errors.Join(retErr, iterator.Close()) }()
	var latest artifact.Entry
	for {
		entries, nextErr := iterator.Next(ctx, 512)
		if nextErr != nil && !errors.Is(nextErr, io.EOF) {
			return artifact.Entry{}, false, nextErr
		}
		if len(entries) > 0 {
			latest = entries[len(entries)-1]
			found = true
		}
		if errors.Is(nextErr, io.EOF) {
			return latest, found, nil
		}
	}
}

func spoolStoreArtifactForServe(
	ctx context.Context,
	store artifact.ArtifactStore,
	origin, kind, name string,
) (_ *artifact.PeerArtifactSpool, retErr error) {
	ref, err := artifact.FromWireRef(origin, artifact.Kind(kind), name)
	if err != nil {
		return nil, err
	}
	entry, reader, err := store.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	if entry.Ref != ref {
		return nil, fmt.Errorf("%w: artifact store returned the wrong reference", artifact.ErrArtifactCorrupt)
	}
	response, err := os.CreateTemp("", "agentsview-peer-wire-response-*")
	if err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			retErr = errors.Join(retErr, response.Close(), os.Remove(response.Name()))
		}
	}()
	if err := response.Chmod(0o600); err != nil {
		return nil, err
	}
	if err := artifact.EncodeWire(ctx, ref, reader, response); err != nil {
		return nil, err
	}
	if err := reader.Verify(); err != nil {
		return nil, fmt.Errorf("%w: %v", artifact.ErrArtifactCorrupt, err)
	}
	info, err := response.Stat()
	if err != nil {
		return nil, err
	}
	if err := response.Sync(); err != nil {
		return nil, err
	}
	if _, err := response.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	cleanup = false
	return &artifact.PeerArtifactSpool{
		Origin: origin, Kind: kind, Name: name,
		Hash: entry.Identity.SHA256, ContentType: "application/octet-stream",
		Size: info.Size(), File: response,
	}, nil
}

func spoolLatestStoreCheckpointForServe(
	ctx context.Context, store artifact.ArtifactStore, origin string,
) (_ *artifact.PeerArtifactSpool, retErr error) {
	latest, found, err := latestStoreCheckpointEntry(ctx, store, origin)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, artifact.ErrArtifactNotFound
	}
	wire, err := artifact.ToWireRef(latest.Ref)
	if err != nil {
		return nil, err
	}
	return spoolStoreArtifactForServe(ctx, store, origin, string(wire.Kind), wire.Name)
}

type artifactCountingReader struct {
	reader io.Reader
	read   int64
}

type serverArtifactImportRetryScheduler struct{ server *Server }

func (s serverArtifactImportRetryScheduler) RecordChanged(
	ctx context.Context, entry artifact.Entry,
) error {
	local, ok := s.server.db.(*db.DB)
	if !ok || local.ReadOnly() || s.server.artifactStore == nil {
		return errors.New("artifact import store is not writable")
	}
	coordinator := artifact.NewStoreImportCoordinator(
		local, s.server.artifactStore, s.server.localArtifactOrigin(),
	)
	if err := coordinator.RecordChanged(ctx, entry); err != nil {
		return err
	}
	s.server.lockSessionLifecycle()
	s.server.artifactImportPending = true
	s.server.sessionLifecycleMu.Unlock()
	return nil
}

func (r *artifactCountingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += int64(n)
	return n, err
}

func (s *Server) humaPostArtifact(
	ctx context.Context,
	in *artifactPostInput,
) (*jsonOutput[artifactPostResponse], error) {
	local, err := s.writableArtifactImportDB()
	if err != nil {
		return nil, err
	}
	store, release, err := s.acquireArtifactStore()
	if err != nil {
		return nil, err
	}
	defer release()
	deferred := in.ImportMode == "deferred"
	ref, err := artifact.FromWireRef(in.Origin, artifact.Kind(in.Kind), in.Name)
	if err != nil {
		return nil, artifactRouteError("post artifact", err)
	}
	wire, err := artifact.ToWireRef(ref)
	if err != nil {
		return nil, artifactRouteError("post artifact", err)
	}
	counting := &artifactCountingReader{reader: in.Body}
	spool, err := artifact.DecodeWireToCanonicalSpool(ctx, wire, counting,
		artifact.PeerWireLimits(wire.Kind))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, artifactRouteError("post artifact", err)
	}
	defer func() { _ = spool.Close() }()
	canonicalRef := spool.Ref()
	identity := spool.Identity()
	repair, repairQueued, err := local.ArtifactRepairForRef(
		ctx, canonicalRef.Origin, string(canonicalRef.Kind), canonicalRef.Name,
	)
	if err != nil {
		return nil, artifactRouteError("post artifact repair lookup", err)
	}
	duplicate := false
	if repairQueued {
		if repair.SHA256 != identity.SHA256 || repair.Size != identity.Size ||
			repair.Origin != canonicalRef.Origin || repair.Kind != string(canonicalRef.Kind) ||
			repair.Name != canonicalRef.Name {
			return nil, artifactRouteError("post artifact repair",
				fmt.Errorf("%w: queued repair identity does not match peer content",
					artifact.ErrArtifactConflict))
		}
		trusted, err := spool.Rewind()
		if err != nil {
			return nil, artifactRouteError("post artifact repair", err)
		}
		if err := artifact.RepairArtifactFromTrustedPeer(
			ctx, local, store, repair, trusted,
			serverArtifactImportRetryScheduler{server: s},
		); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, artifactRouteError("post artifact repair", err)
		}
		duplicate = true
	} else {
		created, err := spool.Create(ctx, store)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, artifactRouteError("post artifact", err)
		}
		duplicate = !created.Created
	}
	if !duplicate {
		coordinator := artifact.NewStoreImportCoordinator(
			local, store, s.localArtifactOrigin(),
		)
		if err := coordinator.RecordChanged(ctx, artifact.Entry{
			Ref: canonicalRef, Identity: identity,
		}); err != nil {
			return nil, artifactRouteError("record changed artifact", err)
		}
	}
	res := artifact.PeerArtifactWrite{
		Origin: in.Origin, Kind: in.Kind, Name: in.Name,
		Hash: identity.SHA256, Size: counting.read,
		Duplicate: duplicate,
	}
	if deferred {
		s.lockSessionLifecycle()
		s.artifactImportPending = true
		s.sessionLifecycleMu.Unlock()
		return artifactPostOutput(res), nil
	}
	s.lockSessionLifecycle()
	defer s.sessionLifecycleMu.Unlock()
	drivesImport := canonicalRef.Kind == artifact.KindCheckpoints ||
		canonicalRef.Kind == artifact.KindMeta
	if drivesImport || s.artifactImportPending {
		importRes, err := s.importPeerArtifacts(ctx, local, store)
		if err != nil {
			return nil, err
		}
		s.artifactImportPending = importRes.Deferred > 0
	}
	return artifactPostOutput(res), nil
}

func (s *Server) postArtifactHTTP(w http.ResponseWriter, r *http.Request) {
	output, err := s.humaPostArtifact(r.Context(), &artifactPostInput{
		Origin:     r.PathValue("origin"),
		Kind:       r.PathValue("kind"),
		Name:       r.PathValue("name"),
		ImportMode: r.Header.Get("X-Agentsview-Artifact-Import"),
		Body:       r.Body,
	})
	if err != nil {
		writeArtifactHTTPError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(output.Body); err != nil && r.Context().Err() == nil {
		log.Printf("encoding artifact POST response: %v", err)
	}
}

func (s *Server) postArtifactExchangeHTTP(w http.ResponseWriter, r *http.Request) {
	if !isLocalhostRequest(r) {
		http.Error(w, "artifact exchange is only available from localhost", http.StatusForbidden)
		return
	}
	local, err := s.writableArtifactImportDB()
	if err != nil {
		writeArtifactHTTPError(w, err)
		return
	}
	store, release, err := s.acquireArtifactStore()
	if err != nil {
		writeArtifactHTTPError(w, err)
		return
	}
	defer release()

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input artifactExchangeRequest
	if err := decoder.Decode(&input); err != nil {
		http.Error(w, "invalid artifact exchange request", http.StatusBadRequest)
		return
	}
	if input.Target == "" {
		http.Error(w, "artifact exchange target is required", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid artifact exchange request", http.StatusBadRequest)
		return
	}
	if err := artifact.ValidateSyncTarget(input.Target); err != nil {
		writeArtifactHTTPError(w, apiError(http.StatusBadRequest,
			"invalid artifact exchange target"))
		return
	}

	s.lockSessionLifecycle()
	defer s.sessionLifecycleMu.Unlock()
	syncOpts := artifact.SyncOptions{
		DataDir:          s.cfg.DataDir,
		Target:           input.Target,
		Origin:           s.localArtifactOrigin(),
		Token:            input.Token,
		AllowInsecure:    input.AllowInsecure,
		BaselineMetadata: input.BaselineMetadata,
		OnDataChanged: func() {
			if s.broadcaster != nil {
				s.broadcaster.Emit("data_changed")
			}
		},
	}
	var result artifact.SyncResult
	if s.artifactRepository != nil {
		result, err = artifact.SyncWithRepository(
			r.Context(), local, s.artifactRepository, syncOpts,
		)
	} else {
		result, err = artifact.SyncWithStore(r.Context(), local, store, syncOpts)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		// The target and peer token are caller-provided secrets. Do not log or
		// reflect transport errors that may include either value.
		writeArtifactHTTPError(w, apiError(http.StatusBadGateway,
			"artifact exchange failed"))
		return
	}
	s.artifactBaselineDone = s.artifactBaselineDone || input.BaselineMetadata
	s.artifactImportPending = false
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(artifactExchangeResponse{
		Origin: result.Origin, ExportedSessions: result.ExportedSessions,
		ImportedSessions: result.ImportedSessions, ImportedMessages: result.ImportedMessages,
		ImportedMetadata: result.ImportedMetadata,
	}); err != nil && r.Context().Err() == nil {
		log.Printf("encoding artifact exchange response: %v", err)
	}
}

func (s *Server) postArtifactResetHTTP(w http.ResponseWriter, r *http.Request) {
	if !isLocalhostRequest(r) {
		http.Error(w, "artifact reset is only available from localhost", http.StatusForbidden)
		return
	}
	local, err := s.writableArtifactImportDB()
	if err != nil {
		writeArtifactHTTPError(w, err)
		return
	}
	ownedStore, resetCtx, err := s.artifactOps.beginReset(r.Context())
	if err != nil {
		writeArtifactHTTPError(w, apiError(http.StatusServiceUnavailable, err.Error()))
		return
	}
	current := s.artifactRepository
	if current == nil {
		_ = s.artifactOps.finishReset(ownedStore)
		writeArtifactHTTPError(w, apiError(http.StatusNotImplemented,
			"artifact reset requires the local AgentsView repository"))
		return
	}

	s.lockSessionLifecycle()
	defer s.sessionLifecycleMu.Unlock()
	origin := s.localArtifactOrigin()
	var pending db.ArtifactResetRepublishPending
	pendingPrepared := false
	fresh, result, resetErr := s.beginArtifactRepositoryReset(
		resetCtx, s.cfg.DataDir, origin, current,
		func() error {
			if origin != "" {
				var err error
				pending, err = artifact.PrepareRepositoryResetRepublish(
					resetCtx, local, s.cfg.DataDir, origin,
				)
				if err != nil {
					return err
				}
				pendingPrepared = true
			}
			commitErr := s.artifactOps.commitResetMutation(resetCtx)
			if commitErr != nil && pendingPrepared {
				_, clearErr := local.ClearArtifactResetRepublishPending(
					context.WithoutCancel(resetCtx), pending,
				)
				return errors.Join(commitErr, clearErr)
			}
			return commitErr
		},
	)
	if resetErr != nil {
		replacement := artifact.ArtifactStore(nil)
		status := http.StatusInternalServerError
		if !current.Closed() {
			replacement = ownedStore
			status = http.StatusServiceUnavailable
		}
		if resetCtx.Err() != nil {
			status = http.StatusServiceUnavailable
		}
		if replacement == nil {
			s.metadata = nil
		}
		finishErr := s.artifactOps.finishReset(replacement)
		http.Error(w, errors.Join(resetErr, finishErr).Error(), status)
		return
	}

	freshStore := fresh.Content()
	if err := s.artifactOps.setResetStore(freshStore); err != nil {
		s.metadata = nil
		finishErr := s.artifactOps.finishReset(nil)
		http.Error(w, errors.Join(err, fresh.Close(), finishErr).Error(), http.StatusInternalServerError)
		return
	}
	s.artifactRepository = fresh
	s.replaceArtifactStoreCursorRegistry()
	s.metadata = artifact.NewMetadataRecorder(local, artifact.MetadataRecorderOptions{
		Origin: s.localArtifactOrigin(),
		Store:  freshStore,
	})
	s.artifactBaselineDone = false
	result, resetErr = s.republishArtifactRepositoryReset(
		resetCtx, s.cfg.DataDir, local, s.localArtifactOrigin(), fresh, result,
	)
	resetErr = errors.Join(resetErr, resetCtx.Err())
	if resetErr != nil {
		finishErr := s.artifactOps.finishReset(freshStore)
		http.Error(w, errors.Join(resetErr, finishErr).Error(), http.StatusInternalServerError)
		return
	}
	s.artifactBaselineDone = true
	if err := s.artifactOps.finishReset(freshStore); err != nil {
		s.metadata = nil
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(artifactResetResponse{
		RepositoryResetResult: result,
		ManualCleanup:         artifact.ArtifactResetManualCleanupWarning,
		ForeignArtifacts:      artifact.ArtifactResetForeignRelayWarning,
	}); err != nil && r.Context().Err() == nil {
		log.Printf("encoding artifact reset response: %v", err)
	}
}

func (s *Server) postArtifactMaintenanceHTTP(w http.ResponseWriter, r *http.Request) {
	if !isLocalhostRequest(r) {
		http.Error(w, "artifact maintenance is only available from localhost", http.StatusForbidden)
		return
	}
	if _, err := s.writableArtifactImportDB(); err != nil {
		writeArtifactHTTPError(w, err)
		return
	}
	store, release, err := s.acquireArtifactStore()
	if err != nil {
		writeArtifactHTTPError(w, err)
		return
	}
	defer release()

	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input artifactMaintenanceRequest
	if err := decoder.Decode(&input); err != nil {
		http.Error(w, "invalid artifact maintenance request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid artifact maintenance request", http.StatusBadRequest)
		return
	}
	grace, graceErr := parseArtifactMaintenanceDuration(input.Grace, input.GraceSeconds)
	quarantineGrace, quarantineGraceErr := parseArtifactMaintenanceDuration(
		input.QuarantineGrace, input.QuarantineGraceSeconds,
	)
	if graceErr != nil || quarantineGraceErr != nil {
		http.Error(w, "invalid artifact maintenance limits", http.StatusBadRequest)
		return
	}
	maintenanceOpts := artifact.ArtifactMaintenanceOptions{
		TrashGrace: grace,
		EmptyTrash: artifact.WorkBudget{
			MaxObjects: input.MaxObjects, Cursor: input.TrashCursor,
		},
		GC: artifact.WorkBudget{
			MaxObjects: input.MaxObjects, MaxBytes: input.MaxBytes,
			Cursor: input.GCCursor,
		},
		Repack: artifact.WorkBudget{
			MaxObjects: input.MaxObjects, MaxBytes: input.MaxBytes,
			Cursor: input.RepackCursor,
		},
	}
	if err := artifact.ValidateArtifactMaintenanceOptions(maintenanceOpts); err != nil {
		http.Error(w, "invalid artifact maintenance limits", http.StatusBadRequest)
		return
	}

	s.lockSessionLifecycle()
	defer s.sessionLifecycleMu.Unlock()
	logical, err := artifact.GarbageCollect(r.Context(), artifact.GCOptions{
		Store:           store,
		Grace:           grace,
		QuarantineGrace: quarantineGrace,
		DryRun:          input.DryRun,
	})
	if err != nil {
		writeArtifactHTTPError(w, artifactRouteError("artifact retention", err))
		return
	}
	response := artifactMaintenanceResponse{Logical: logical}
	if s.artifactRepository != nil && !input.DryRun {
		response.Physical.Supported = true
		response.Physical.Result, err = s.artifactRepository.RunMaintenance(
			r.Context(), maintenanceOpts)
		if err != nil {
			writeArtifactHTTPError(w, artifactRouteError("artifact physical maintenance", err))
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil && r.Context().Err() == nil {
		log.Printf("encoding artifact maintenance response: %v", err)
	}
}

func writeArtifactHTTPError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	status := http.StatusInternalServerError
	message := "internal error"
	var apiErr *apiErrorResponse
	if errors.As(err, &apiErr) {
		status = apiErr.Status
		message = apiErr.Message
	}
	http.Error(w, message, status)
}

func artifactPostOutput(res artifact.PeerArtifactWrite) *jsonOutput[artifactPostResponse] {
	return &jsonOutput[artifactPostResponse]{
		Body: artifactPostResponse{
			Origin:    res.Origin,
			Kind:      res.Kind,
			Name:      res.Name,
			Hash:      res.Hash,
			Size:      res.Size,
			Duplicate: res.Duplicate,
		},
	}
}

func (s *Server) humaFinalizeArtifacts(
	ctx context.Context,
	_ *emptyInput,
) (*jsonOutput[artifactFinalizeResponse], error) {
	local, err := s.writableArtifactImportDB()
	if err != nil {
		return nil, err
	}
	store, release, err := s.acquireArtifactStore()
	if err != nil {
		return nil, err
	}
	defer release()

	s.lockSessionLifecycle()
	defer s.sessionLifecycleMu.Unlock()
	res, err := s.importPeerArtifacts(ctx, local, store)
	if err != nil {
		return nil, err
	}
	s.artifactImportPending = res.Deferred > 0
	return &jsonOutput[artifactFinalizeResponse]{
		Body: artifactFinalizeResponse{
			ImportedSessions: res.Sessions,
			ImportedMessages: res.Messages,
			ImportedMetadata: res.Metadata,
			Deferred:         res.Deferred,
		},
	}, nil
}

func (s *Server) writableArtifactImportDB() (*db.DB, error) {
	if err := s.requireLocalArtifactStore(); err != nil {
		return nil, err
	}
	return s.db.(*db.DB), nil
}

func (s *Server) requireLocalArtifactStore() error {
	local, ok := s.db.(*db.DB)
	if !ok {
		return apiError(http.StatusNotImplemented,
			"artifact routes are not available in remote mode")
	}
	if local.ReadOnly() {
		return apiError(http.StatusNotImplemented,
			"artifact routes are not available in read-only mode")
	}
	return nil
}

func (s *Server) hasWritableArtifactStore() bool {
	local, ok := s.db.(*db.DB)
	return ok && !local.ReadOnly() && s.artifactStore != nil
}

func (s *Server) importPeerArtifacts(
	ctx context.Context, local *db.DB, store artifact.ArtifactStore,
) (artifact.ImportResult, error) {
	localOrigin := s.cfg.ArtifactOriginID
	if localOrigin == "" {
		var err error
		localOrigin, err = artifact.EnsureOrigin(local)
		if err != nil {
			return artifact.ImportResult{}, internalError("artifact import origin", err)
		}
	}
	coordinator := artifact.NewStoreImportCoordinator(local, store, localOrigin)
	res, err := coordinator.Finalize(ctx)
	if err != nil {
		return artifact.ImportResult{}, artifactRouteError("import peer artifacts", err)
	}
	if res.Changed() && s.broadcaster != nil {
		// Imports add sessions and apply curation metadata, so live
		// clients need the session-index refresh that only the
		// "sessions" scope triggers; "messages" additionally
		// invalidates hydrated session details and cached signal
		// detail. Emit "sessions" last so a coalesced burst resolves
		// to the index refresh.
		s.broadcaster.Emit("messages")
		s.broadcaster.Emit("sessions")
	}
	return res, nil
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
