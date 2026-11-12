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
	read := func(name string) ([]byte, error) {
		for _, root := range roots {
			if within(root, name) {
				return readPiFile(ctx, root, name, id)
			}
		}
		return nil, errors.New("Pi source outside configured root")
	}
	name, err := uniqueLocalSource(ctx, provider, stored, func(name string) (bool, error) {
		_, err := read(name)
		return err == nil, ctx.Err()
	})
	if err != nil {
		return "", nil, err
	}
	data, err := read(name)
	return name, data, err
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
	return slices.Concat(data[:start], encoded, data[end:]), nil
}

func piDestination(cwd, basename string) (string, error) {
	dir := os.Getenv("PI_CODING_AGENT_SESSION_DIR")
	flat := dir != ""
	if !flat {
		dir = os.Getenv("PI_CODING_AGENT_DIR")
		if dir == "" {
			dir = "~/.pi/agent"
		}
	}
	expanded, err := pathutil.ExpandHome(dir)
	if err != nil {
		return "", err
	}
	dir, err = filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
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
