package remotesync

import (
	"compress/gzip"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/parser"
)

// HTTPSource fetches remote sync data from a remote agentsview daemon.
type HTTPSource struct {
	URL    string
	Token  string
	Client *http.Client
}

func (s HTTPSource) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	// Archive responses can stream for hours; ctx governs their lifetime.
	return &http.Client{Timeout: 0}
}

// Archive requests an archive and returns its body once the daemon accepts.
func (s HTTPSource) Archive(ctx context.Context, request ArchiveRequest) (*ArchiveStream, error) {
	resp, err := s.requestArchive(ctx, request)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, httpStatusError(resp)
	}
	if err := ValidateProtocolHeader(resp.Header); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	// The daemon negotiates zstd or gzip from Accept-Encoding. Extraction
	// rejects any other value, so it is passed through unchanged.
	return &ArchiveStream{
		Body:     resp.Body,
		Encoding: ArchiveEncoding(resp.Header.Get("Content-Encoding")),
		Size:     positiveContentLength(resp.ContentLength),
	}, nil
}

func positiveContentLength(n int64) int64 {
	if n > 0 {
		return n
	}
	return 0
}

func (s HTTPSource) Manifest(
	ctx context.Context, targets TargetSet,
) (Manifest, bool, error) {
	body := generatedTargets(targets)
	resp, err := apiclient.RawRequest(s.URL, s.client(), func(api *apiclient.Client) error {
		_, err := api.PostAPIV1RemoteSyncManifestWithResponse(ctx, &apiclient.PostAPIV1RemoteSyncManifestRequestOptions{Body: &body})
		return err
	}, s.transferHeaders)
	if err != nil {
		return Manifest{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotImplemented {
		if err := ValidateProtocolHeader(resp.Header); err != nil {
			return Manifest{}, false, err
		}
		return Manifest{}, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Manifest{}, false, httpStatusError(resp)
	}
	if err := ValidateProtocolHeader(resp.Header); err != nil {
		return Manifest{}, false, err
	}
	bodyReader := &httpBodyReader{r: resp.Body}
	reader := io.Reader(bodyReader)
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(bodyReader)
		if err != nil {
			if bodyReader.err != nil {
				err = bodyReader.err
			}
			return Manifest{}, false, fmt.Errorf("decode manifest gzip: %w", err)
		}
		defer gz.Close()
		reader = gz
	}
	var manifest Manifest
	if err := json.UnmarshalRead(reader, &manifest); err != nil {
		if bodyReader.err != nil {
			err = bodyReader.err
		}
		return Manifest{}, false, fmt.Errorf("decode remote manifest: %w", err)
	}
	return manifest, true, nil
}

func (s HTTPSource) Targets(
	ctx context.Context,
) (TargetSet, error) {
	resp, err := apiclient.RawRequest(s.URL, s.client(), func(api *apiclient.Client) error {
		_, err := api.GetAPIV1RemoteSyncTargetsWithResponse(ctx, &apiclient.GetAPIV1RemoteSyncTargetsRequestOptions{})
		return err
	}, s.transferHeaders)
	if err != nil {
		return TargetSet{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return TargetSet{}, httpStatusError(resp)
	}
	if err := ValidateProtocolHeader(resp.Header); err != nil {
		return TargetSet{}, err
	}
	var targets TargetSet
	bodyReader := &httpBodyReader{r: resp.Body}
	if err := json.UnmarshalRead(bodyReader, &targets); err != nil {
		if bodyReader.err != nil {
			err = bodyReader.err
		}
		return TargetSet{}, fmt.Errorf("decode remote targets: %w", err)
	}
	return targets, nil
}

func (s HTTPSource) transferHeaders(_ context.Context, req *http.Request) error {
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	SetProtocolHeader(req.Header)
	if req.Method == http.MethodPost {
		req.Header.Set("Accept-Encoding", "zstd, gzip")
	}
	return nil
}

func generatedProviderPaths(paths map[parser.AgentType][]string) map[string][]string {
	result := make(map[string][]string, len(paths))
	for provider, paths := range paths {
		result[string(provider)] = paths
	}
	return result
}

func generatedTargets(targets TargetSet) apiclient.RemotesyncTargetSet {
	return apiclient.RemotesyncTargetSet{
		Dirs: generatedProviderPaths(targets.Dirs), Files: generatedProviderPaths(targets.Files),
		ProviderExtraFiles: generatedProviderPaths(targets.ProviderExtraFiles), ExtraFiles: targets.ExtraFiles,
		CodexIndexFiles: targets.CodexIndexFiles, ForbiddenRoots: targets.ForbiddenRoots,
	}
}

func (s HTTPSource) requestArchive(
	ctx context.Context, request ArchiveRequest,
) (*http.Response, error) {
	targets := generatedTargets(request.TargetSet)
	body := apiclient.RemotesyncArchiveRequest{
		Dirs: targets.Dirs, Files: targets.Files, ProviderExtraFiles: targets.ProviderExtraFiles,
		ExtraFiles: targets.ExtraFiles, CodexIndexFiles: targets.CodexIndexFiles, ForbiddenRoots: targets.ForbiddenRoots,
	}
	if request.DeltaFiles != nil {
		body.DeltaFiles = new(request.DeltaFiles)
	}
	return apiclient.RawRequest(s.URL, s.client(), func(api *apiclient.Client) error {
		_, err := api.PostAPIV1RemoteSyncArchiveWithResponse(ctx, &apiclient.PostAPIV1RemoteSyncArchiveRequestOptions{Body: &body})
		return err
	}, s.transferHeaders)
}

// StatusError reports a non-2xx response from a remote daemon's
// remote-sync endpoints. Detail carries the (untrusted) response
// body for local logs; user-facing summaries should rely on Code.
type StatusError struct {
	Code   int
	Status string
	Detail string
}

func (e *StatusError) Error() string {
	msg := e.Detail
	if msg == "" {
		msg = e.Status
	}
	return fmt.Sprintf("remote sync %s: %s", e.Status, msg)
}

func httpStatusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &StatusError{
		Code:   resp.StatusCode,
		Status: resp.Status,
		Detail: strings.TrimSpace(string(body)),
	}
}
