package sessionmaterialize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/pathutil"
	"go.kenn.io/agentsview/internal/remotesync"
)

func materializePi(ctx context.Context, cfg config.Config, session *db.Session, cwd string) (Result, error) {
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return Result{}, err
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return Result{}, errors.New("cwd must be an existing directory")
	}
	host, raw := parser.StripHostPrefix(session.ID)
	id, ok := strings.CutPrefix(raw, "pi:")
	if !ok || (id == "" || strings.ContainsAny(id, "~\x00")) {
		return Result{}, errors.New("invalid native Pi session identity")
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
		data, err = readPiFile(ctx, root, name, id)
		err = errors.Join(err, lock.Close())
	} else {
		name, data, err = findPiSource(ctx, cfg, session, source, id)
		source = name
	}
	if err != nil {
		return Result{}, fmt.Errorf("reading native Pi transcript: %w", err)
	}
	data, err = relocatePiHeader(data, id, filepath.Base(name), cwd)
	if err != nil {
		return Result{}, err
	}
	dest, err := piDestination(cwd, filepath.Base(name))
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
	if ref, found, err := provider.FindSource(ctx, parser.FindSourceRequest{StoredFilePath: stored}); err != nil {
		return "", nil, err
	} else if found {
		for _, root := range roots {
			if within(root, ref.DisplayPath) {
				if data, err := readPiFile(ctx, root, ref.DisplayPath, id); err == nil {
					return ref.DisplayPath, data, nil
				}
			}
		}
	}
	discoverer, ok := provider.(parser.StreamingDiscoverer)
	if !ok {
		return "", nil, errors.New("Pi source discovery unavailable")
	}
	var name string
	var data []byte
	var foundInfo os.FileInfo
	err := discoverer.DiscoverEach(ctx, func(ref parser.SourceRef) error {
		for _, root := range roots {
			if !within(root, ref.DisplayPath) {
				continue
			}
			candidate, err := readPiFile(ctx, root, ref.DisplayPath, id)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				continue
			}
			info, err := os.Stat(ref.DisplayPath)
			if err != nil {
				return err
			}
			if foundInfo != nil && !os.SameFile(foundInfo, info) {
				return errors.New("ambiguous native Pi transcript")
			}
			name, data, foundInfo = ref.DisplayPath, candidate, info
			break
		}
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	if foundInfo == nil {
		return "", nil, errors.New("native Pi transcript is unavailable from configured sources")
	}
	return name, data, nil
}

func readPiFile(ctx context.Context, root, name, id string) ([]byte, error) {
	if !within(root, name) || !strings.HasSuffix(name, ".jsonl") {
		return nil, errors.New("Pi source outside configured root")
	}
	data, before, err := readFile(ctx, name, maxSourceBytes)
	if err != nil {
		return nil, err
	}
	if _, err := relocatePiHeader(data, id, filepath.Base(name), ""); err != nil {
		return nil, err
	}
	after, err := os.Stat(name)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		return nil, errors.New("Pi source changed while reading; retry when idle")
	}
	return data, nil
}

// relocatePiHeader changes only the cwd value, keeping the rest of the JSONL byte-for-byte.
func relocatePiHeader(data []byte, id, basename, cwd string) ([]byte, error) {
	line, _, _ := bytes.Cut(data, []byte{'\n'})
	var header struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(line, &header); err != nil || header.Type != "session" {
		return nil, errors.New("invalid Pi session header")
	}
	if (header.ID != "" && header.ID != id) || (header.ID == "" && strings.TrimSuffix(basename, ".jsonl") != id) {
		return nil, errors.New("Pi session identity differs")
	}
	if cwd == "" {
		return data, nil
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
		offset := int(decoder.InputOffset())
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if key == "cwd" {
			count++
			colon := bytes.IndexByte(line[offset:], ':')
			start = offset + colon + 1
			for start < len(line) && (line[start] == ' ' || line[start] == '\t' || line[start] == '\r') {
				start++
			}
			end = int(decoder.InputOffset())
		}
	}
	if count != 1 {
		return nil, errors.New("Pi session header requires one cwd")
	}
	encoded, err := json.Marshal(cwd)
	if err != nil {
		return nil, err
	}
	result := make([]byte, 0, len(data)+len(encoded)-(end-start))
	result = append(result, data[:start]...)
	result = append(result, encoded...)
	result = append(result, data[end:]...)
	return result, nil
}

func piDestination(cwd, basename string) (string, error) {
	if basename == "." || basename == "" || filepath.Base(basename) != basename {
		return "", errors.New("invalid Pi source filename")
	}
	dir := os.Getenv("PI_CODING_AGENT_SESSION_DIR")
	if dir != "" {
		expanded, err := pathutil.ExpandHome(dir)
		if err != nil {
			return "", err
		}
		dir, err = filepath.Abs(expanded)
		if err != nil {
			return "", err
		}
	} else {
		dir = os.Getenv("PI_CODING_AGENT_DIR")
		if dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			dir = filepath.Join(home, ".pi", "agent")
		}
		expanded, err := pathutil.ExpandHome(dir)
		if err != nil {
			return "", err
		}
		dir, err = filepath.Abs(expanded)
		if err != nil {
			return "", err
		}
		encoded := cwd
		if strings.HasPrefix(encoded, "/") || strings.HasPrefix(encoded, `\`) {
			encoded = encoded[1:]
		}
		encoded = strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(encoded)
		dir = filepath.Join(dir, "sessions", "--"+encoded+"--")
	}
	return filepath.Join(dir, basename), nil
}
