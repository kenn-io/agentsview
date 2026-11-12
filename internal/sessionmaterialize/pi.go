package sessionmaterialize

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/ctxio"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/pathutil"
	"go.kenn.io/agentsview/internal/remotesync"
)

func materializePi(ctx context.Context, cfg config.Config, session *db.Session, cwd string) (Result, error) {
	host, raw := parser.StripHostPrefix(session.ID)
	id, ok := strings.CutPrefix(raw, "pi:")
	if !ok || (id == "" || strings.ContainsAny(id, "~\x00")) {
		return Result{}, errors.New("invalid native Pi session identity")
	}
	cwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return Result{}, fmt.Errorf("resolve Pi checkout: %w", err)
	}
	source, err := storedSource(session, host)
	if err != nil {
		return Result{}, err
	}
	if strings.HasPrefix(source, "archive://") || (host != "" && strings.HasPrefix(source, host+":archive://")) {
		return Result{}, errors.New("Pi keeps no retained or attached captures")
	}
	var name string
	var data []byte
	if host != "" {
		if !slices.ContainsFunc(cfg.RemoteHosts, func(r config.RemoteHost) bool { return r.Host == host }) {
			return Result{}, errors.New("session host is not configured")
		}
		remote, ok := strings.CutPrefix(source, host+":")
		if !ok || remote == "" {
			return Result{}, errors.New("mirror source host does not match session identity")
		}
		root := remotesync.MirrorDir(cfg.DataDir, host)
		name, err = remotesync.SafeRemappedRemotePath(root, remote)
		if err != nil {
			return Result{}, err
		}
		lock, err := remotesync.AcquireMirrorLock(ctx, root)
		if err != nil {
			return Result{}, err
		}
		data, err = readPiFile(ctx, root, name)
		err = errors.Join(err, lock.Close())
	} else {
		name, data, err = findPiSource(ctx, cfg, session, source, id)
		source = name
	}
	if err != nil {
		return Result{}, fmt.Errorf("reading native Pi transcript: %w", err)
	}
	data, err = relocatePiHeader(ctx, data, id, cwd)
	if err != nil {
		return Result{}, err
	}
	basename := filepath.Base(name)
	if !strings.HasSuffix(basename, "_"+id+".jsonl") {
		if !parser.IsValidSessionID(id) || !filepath.IsLocal(id+".jsonl") {
			return Result{}, errors.New("Pi session identity cannot be used as a filename")
		}
		basename = id + ".jsonl"
	}
	dest, err := piDestination(cwd, basename)
	if err != nil {
		return Result{}, err
	}
	changed, err := publishFiles(ctx, []installFile{{dest, data}})
	if err != nil {
		return Result{}, err
	}
	status := "unchanged"
	if changed {
		status = "installed"
	}
	return Result{Source: source, Paths: []string{dest}, Cwd: cwd, Status: status}, nil
}

func findPiSource(ctx context.Context, cfg config.Config, session *db.Session, stored, id string) (string, []byte, error) {
	roots := configuredRoots(cfg, session, parser.AgentPi)
	provider, ok := parser.NewProvider(parser.AgentPi, parser.ProviderConfig{Roots: roots, Machine: cfg.InstallationID, SourceMachines: cfg.SourceMachines[parser.AgentPi]})
	if !ok {
		return "", nil, errors.New("Pi source provider unavailable")
	}
	rootFor := func(name string) string {
		for _, root := range roots {
			if within(root, name) {
				return root
			}
		}
		return ""
	}
	candidates := parser.NewJSONLSourceSet(parser.AgentPi, roots, parser.WithRecursive(), parser.WithSymlinkFollowing())
	name, err := uniqueLocalSourceWithDiscovery(ctx, provider, stored, candidates.DiscoverEach, func(name string) (bool, error) {
		root := rootFor(name)
		if root == "" {
			return false, errors.New("Pi source outside configured root")
		}
		err := checkPiHeaderFile(ctx, name, id)
		if errors.Is(err, errPiIdentity) || errors.Is(err, errPiNotSession) {
			return false, nil
		}
		return err == nil, err
	})
	if err != nil {
		return "", nil, err
	}
	data, err := readPiFile(ctx, rootFor(name), name)
	return name, data, err
}

func readPiFile(ctx context.Context, root, name string) ([]byte, error) {
	if !within(root, name) || !strings.HasSuffix(name, ".jsonl") {
		return nil, errors.New("Pi source outside configured root")
	}
	info, err := os.Stat(name)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxSourceBytes {
		return nil, errors.New("native source exceeds size limit")
	}
	data, before, err := readFile(ctx, name, maxSourceBytes)
	if err != nil {
		return nil, err
	}
	after, err := os.Stat(name)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		return nil, errors.New("Pi source changed while reading; retry when idle")
	}
	return data, nil
}

var errPiIdentity = errors.New("Pi session identity differs")
var errPiNotSession = errors.New("invalid Pi session header; first parsed record is not a session")

// Pi scans at most 1 MiB to identify a session before its full-load fallback.
// The bounded lookup fails closed when a candidate cannot be identified.
func piHeader(ctx context.Context, r io.Reader, id string) ([]byte, int, error) {
	reader := bufio.NewReader(io.LimitReader(ctxio.Reader{Context: ctx, Reader: r}, 1<<20+1))
	offset := 0
	for {
		line, err := reader.ReadBytes('\n')
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, ctxErr
		}
		start := offset
		offset += len(line)
		if offset > 1<<20 {
			return nil, 0, errors.New("Pi session header exceeds 1 MiB lookup limit")
		}
		var header struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Version *int   `json:"version"`
		}
		if json.Valid(line) {
			if err := json.Unmarshal(line, &header); err != nil {
				return nil, 0, errors.New("invalid Pi session header fields")
			}
			if header.Type != "session" {
				return nil, 0, errPiNotSession
			}
			if header.ID == "" {
				return nil, 0, errors.New("Pi session header has no id; current Pi cannot resume this file")
			}
			if header.ID != id {
				return nil, 0, errPiIdentity
			}
			if header.Version != nil && (*header.Version < 1 || *header.Version > 3) {
				return nil, 0, errors.New("unsupported Pi session version; current Pi format is version 3")
			}
			return line, start, nil
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return nil, 0, err
			}
			return nil, 0, errors.New("invalid Pi session header")
		}
	}
}

func checkPiHeaderFile(ctx context.Context, name, id string) (retErr error) {
	file, err := os.Open(name)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); err != nil {
			retErr = fmt.Errorf("close Pi source: %w", err)
		}
	}()
	before, err := file.Stat()
	if err != nil {
		return err
	}
	_, _, err = piHeader(ctx, file, id)
	after, statErr := os.Stat(name)
	if statErr != nil || !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		return errors.New("Pi source changed while reading; retry when idle")
	}
	return err
}

// relocatePiHeader changes only the cwd value, keeping the rest of the JSONL byte-for-byte.
func relocatePiHeader(ctx context.Context, data []byte, id, cwd string) ([]byte, error) {
	line, offset, err := piHeader(ctx, bytes.NewReader(data), id)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	start, end, count := -1, -1, 0
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if key == "cwd" {
			count++
			end = int(decoder.InputOffset())
			start = end - len(value)
		}
	}
	if count != 1 {
		return nil, errors.New("Pi session header requires one cwd")
	}
	encoded, _ := json.Marshal(cwd)
	return slices.Concat(data[:offset+start], encoded, data[offset+end:]), nil
}

func piDestination(cwd, basename string) (string, error) {
	def, _ := parser.AgentByType(parser.AgentPi)
	dir := os.Getenv(def.NativeEnvVar)
	flat := dir != ""
	if !flat {
		dir = os.Getenv(def.DefaultRootEnvVar)
		if dir == "" {
			dir = "~/" + def.DefaultRootDir
		}
	}
	if strings.HasPrefix(dir, "file://") {
		u, err := url.Parse(dir)
		if err != nil || u.Path == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return "", errors.New("invalid Pi directory file URL")
		}
		escaped := strings.ToLower(u.EscapedPath())
		if strings.Contains(escaped, "%2f") || (runtime.GOOS == "windows" && strings.Contains(escaped, "%5c")) {
			return "", errors.New("invalid Pi directory file URL")
		}
		if u.Host != "" && u.Host != "localhost" {
			if runtime.GOOS != "windows" {
				return "", errors.New("Pi directory file URL must be local")
			}
			dir = "//" + u.Host + u.Path
		} else {
			dir = u.Path
			if runtime.GOOS == "windows" && len(dir) >= 3 && dir[0] == '/' && dir[2] == ':' {
				dir = dir[1:]
			}
		}
		dir = filepath.FromSlash(dir)
	}
	expanded, err := pathutil.ExpandHome(dir)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(expanded) {
		expanded = filepath.Join(cwd, expanded)
	}
	dir = filepath.Clean(expanded)
	if !flat {
		encoded := cwd
		if strings.HasPrefix(encoded, "/") || strings.HasPrefix(encoded, `\`) {
			encoded = encoded[1:]
		}
		encoded = strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(encoded)
		dir = filepath.Join(dir, "sessions", "--"+encoded+"--")
	}
	return filepath.Join(dir, basename), nil
}
