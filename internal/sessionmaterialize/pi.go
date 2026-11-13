package sessionmaterialize

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
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
		lock, lockErr := remotesync.AcquireMirrorLock(ctx, root)
		if lockErr != nil {
			return Result{}, lockErr
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
	data, err = relocatePiHeader(ctx, data, name, id, cwd)
	if err != nil {
		return Result{}, err
	}
	basename := filepath.Base(name)
	if !strings.HasSuffix(basename, "_"+id+".jsonl") {
		if strings.Trim(id, ".") != id || !parser.IsValidSessionID(strings.ReplaceAll(id, ".", "")) || !filepath.IsLocal(id+".jsonl") {
			return Result{}, errors.New("native Pi session identity cannot be used as a filename")
		}
		basename = id + ".jsonl"
	}
	dest, err := piDestination(ctx, cwd, basename)
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
	provider, ok := parser.NewProvider(parser.AgentPi, parser.ProviderConfig{Roots: localRoots(cfg, parser.AgentPi)})
	if !ok {
		return "", nil, errors.New("native Pi source provider unavailable")
	}
	candidates := parser.NewPiSourceSet(parser.AgentPi, roots)
	name, err := uniqueLocalSource(ctx, provider, stored, scopedDiscover(cfg, session, parser.AgentPi, candidates.DiscoverEach), func(name string) (bool, error) {
		err := checkPiHeaderFile(ctx, name, id)
		if errors.Is(err, errPiIdentity) || errors.Is(err, errPiNotSession) {
			return false, nil
		}
		return err == nil, err
	})
	if err != nil {
		return "", nil, err
	}
	var boundary string
	for _, root := range roots {
		if within(root, name) {
			boundary = root
			break
		}
	}
	if boundary == "" {
		// A recorded path after relabeling or an external link may be outside
		// the machine's roots. Confine the read to its resolved directory.
		resolved, resolveErr := filepath.EvalSymlinks(name)
		if resolveErr != nil {
			return "", nil, resolveErr
		}
		boundary = filepath.Dir(resolved)
	}
	data, err := readPiFile(ctx, boundary, name)
	return name, data, err
}

func readPiFile(ctx context.Context, root, name string) ([]byte, error) {
	if !within(root, name) || !strings.HasSuffix(name, ".jsonl") {
		return nil, errors.New("native Pi source outside configured root")
	}
	data, before, err := readFile(ctx, name, maxSourceBytes)
	if err != nil {
		return nil, err
	}
	after, err := os.Stat(name)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		return nil, errors.New("native Pi source changed while reading; retry when idle")
	}
	return data, nil
}

var (
	errPiIdentity   = errors.New("native Pi session identity differs")
	errPiNotSession = errors.New("invalid Pi session header; first parsed record is not a session")
	errPiHeaderSize = errors.New("native Pi session header exceeds lookup limit")
)

func piHeader(ctx context.Context, r io.Reader, name, id string, limit int64) ([]byte, int, error) {
	reader := bufio.NewReader(io.LimitReader(ctxio.Reader{Context: ctx, Reader: r}, limit+1))
	offset := 0
	for {
		line, err := reader.ReadBytes('\n')
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, ctxErr
		}
		start := offset
		offset += len(line)
		if int64(offset) > limit {
			return nil, 0, errPiHeaderSize
		}
		if jsontext.Value(line).IsValid(jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)) {
			var value any
			decodeErr := json.Unmarshal(line, &value, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
			falsy := decodeErr == nil && (value == nil || value == false || value == "" || value == float64(0))
			if falsy {
				if err == nil {
					continue
				}
			} else {
				var header map[string]jsontext.Value
				if json.Unmarshal(line, &header, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)) != nil {
					return nil, 0, errPiNotSession
				}
				var kind, headerID string
				var version *int
				if json.Unmarshal(header["type"], &kind, jsontext.AllowInvalidUTF8(true)) != nil || kind != "session" {
					return nil, 0, errPiNotSession
				}
				if value, ok := header["id"]; ok && json.Unmarshal(value, &headerID, jsontext.AllowInvalidUTF8(true)) != nil {
					return nil, 0, errPiNotSession
				}
				_, hasID := header["id"]
				if headerID != id && !(!hasID && strings.TrimSuffix(filepath.Base(name), ".jsonl") == id) {
					return nil, 0, errPiIdentity
				}
				if value, ok := header["version"]; ok && json.Unmarshal(value, &version) != nil {
					return nil, 0, errPiNotSession
				}
				if version != nil && (*version < 1 || *version > 3) {
					return nil, 0, errors.New("unsupported Pi session version; Agentsview supports versions 1 through 3")
				}
				return line, start, nil
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return nil, 0, err
			}
			return nil, 0, errPiNotSession
		}
	}
}

func checkPiHeaderFile(ctx context.Context, name, id string) error {
	file, err := os.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	_, _, err = piHeader(ctx, file, name, id, 1<<20)
	return err
}

// relocatePiHeader updates native resume metadata while preserving the remaining bytes.
func relocatePiHeader(ctx context.Context, data []byte, name, id, cwd string) ([]byte, error) {
	line, offset, err := piHeader(ctx, bytes.NewReader(data), name, id, maxSourceBytes)
	if err != nil {
		return nil, err
	}
	decoder := jsontext.NewDecoder(bytes.NewReader(line), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	if _, err := decoder.ReadToken(); err != nil {
		return nil, err
	}
	start, end, count := -1, -1, 0
	hasID := false
	for decoder.PeekKind() != jsontext.KindEndObject {
		key, err := decoder.ReadToken()
		if err != nil {
			return nil, err
		}
		name := key.String()
		value, err := decoder.ReadValue()
		if err != nil {
			return nil, err
		}
		if name == "id" {
			hasID = true
		}
		if name == "cwd" {
			count++
			end = int(decoder.InputOffset())
			start = end - len(value)
		}
	}
	if count != 1 {
		return nil, errors.New("native Pi session header requires one cwd")
	}
	encoded, err := json.Marshal(cwd)
	if err != nil {
		return nil, fmt.Errorf("native Pi checkout path is not valid UTF-8: %w", err)
	}
	if !hasID {
		encodedID, err := json.Marshal(id)
		if err != nil {
			return nil, err
		}
		encoded = slices.Concat(encoded, []byte(`,"id":`), encodedID)
	}
	return slices.Concat(data[:offset+start], encoded, data[offset+end:]), nil
}

func piDestination(ctx context.Context, cwd, basename string) (string, error) {
	def, _ := parser.AgentByType(parser.AgentPi)
	agentDir, err := piResolveDir(cwd, cmp.Or(os.Getenv(def.DefaultRootEnvVar), "~/"+def.DefaultRootDir))
	if err != nil {
		return "", err
	}
	dir := os.Getenv(def.NativeEnvVar)
	if dir == "" {
		dir, err = piSettingsSessionDir(ctx, agentDir, cwd, filepath.Dir(def.DefaultRootDir))
		if err != nil {
			return "", err
		}
	}
	if dir == "" {
		encoded := strings.TrimPrefix(strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(cwd), "-")
		component := "--" + encoded + "--"
		if len(component) > 200 {
			hash := sha256.Sum256([]byte(cwd))
			component = fmt.Sprintf("--%x--", hash[:16])
		}
		return filepath.Join(agentDir, "sessions", component, basename), nil
	}
	dir, err = piResolveDir(cwd, dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, basename), nil
}

// Pi merges global and project settings, with the project value taking precedence.
func piSettingsSessionDir(ctx context.Context, agentDir, cwd, projectConfig string) (string, error) {
	var dir string
	for _, name := range []string{filepath.Join(agentDir, "settings.json"), filepath.Join(cwd, projectConfig, "settings.json")} {
		data, _, err := readFile(ctx, name, 8<<20)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read Pi settings: %w", err)
		}
		var settings map[string]jsontext.Value
		if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &settings, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)); err != nil {
			return "", fmt.Errorf("invalid Pi settings: %w", err)
		}
		if value, ok := settings["sessionDir"]; ok {
			dir = ""
			if err := json.Unmarshal(value, &dir, jsontext.AllowInvalidUTF8(true)); err != nil {
				return "", errors.New("native Pi settings sessionDir must be a string")
			}
		}
	}
	return dir, nil
}

func piResolveDir(cwd, dir string) (string, error) {
	if runtime.GOOS == "windows" {
		dir = piWindowsShellPath(dir)
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
				return "", errors.New("native Pi directory file URL must be local")
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
	return filepath.Clean(expanded), nil
}

// piWindowsShellPath matches Pi's MSYS, Cygwin and WSL drive spellings.
func piWindowsShellPath(dir string) string {
	if !strings.HasPrefix(dir, "/") || strings.HasPrefix(dir, "//") || strings.Contains(dir, `\`) {
		return dir
	}
	part := strings.TrimPrefix(dir, "/")
	if len(part) >= 4 && strings.EqualFold(part[:4], "mnt/") {
		part = part[4:]
	} else if len(part) >= 9 && strings.EqualFold(part[:9], "cygdrive/") {
		part = part[9:]
	}
	if len(part) == 0 || (part[0] < 'a' || part[0] > 'z') && (part[0] < 'A' || part[0] > 'Z') || len(part) > 1 && part[1] != '/' {
		return dir
	}
	return strings.ToUpper(part[:1]) + `:\` + strings.ReplaceAll(strings.TrimPrefix(part[1:], "/"), "/", `\`)
}
