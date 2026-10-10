package rawarchive

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
)

func captureFixture(t *testing.T) CaptureOptions {
	t.Helper()
	data := t.TempDir()
	database, err := db.OpenIsolatedContext(t.Context(), filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.Close())
	root := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(root, "projects", "project-a", "saved.jsonl"), []byte("original bytes\n"))
	return CaptureOptions{DataDir: data, Destination: filepath.Join(t.TempDir(), "capture"), Roots: []RootSpec{{Provider: "claude", Path: filepath.Join(root, "projects")}}, Settings: RecoverySettings{LocalMachineName: "source-device"}, ReaderBuild: "test-build"}
}

func TestCaptureIdentityContinuity(t *testing.T) {
	for _, condition := range []string{"unchanged", "descriptor-only", "changed-payload", "application-directory-moved"} {
		t.Run(condition, func(t *testing.T) {
			ctx := t.Context()
			opts := captureFixture(t)
			first, err := Capture(ctx, opts)
			require.NoError(t, err)
			assert.NoFileExists(t, filepath.Join(opts.DataDir, "telemetry-install-id"))
			require.Len(t, first.NewIdentities, 2)
			identity := filepath.Join(opts.Destination, "capture.json")
			var archive *Archive
			original, err := filepath.EvalSymlinks(opts.DataDir)
			require.NoError(t, err)
			switch condition {
			case "descriptor-only":
				data, err := os.ReadFile(identity)
				require.NoError(t, err)
				identity = filepath.Join(t.TempDir(), "capture.json")
				require.NoError(t, os.WriteFile(identity, data, 0o600))
				require.NoError(t, os.RemoveAll(opts.Destination))
			case "changed-payload":
				dbtest.WriteTestFile(t, filepath.Join(opts.Destination, first.Source.Roots[0].Path, "projects", "project-a", "saved.jsonl"), []byte("changed old copy"))
			case "application-directory-moved":
				spec, err := LoadImportSpec(ctx, identity)
				require.NoError(t, err)
				database := dbtest.OpenTestDB(t)
				require.NoError(t, database.EnableArchiveOnly(ctx))
				archive, err = Open(ctx, database, t.TempDir(), nil)
				require.NoError(t, err)
				defer archive.Close()
				_, err = archive.Import(ctx, spec)
				require.NoError(t, err)
				moved := filepath.Join(t.TempDir(), "moved")
				require.NoError(t, os.Rename(original, moved))
				opts.DataDir, err = filepath.EvalSymlinks(moved)
				require.NoError(t, err)
			}
			if condition == "descriptor-only" || condition == "changed-payload" {
				_, err = LoadImportSpec(ctx, identity)
				require.Error(t, err)
			}
			opts.IdentityFrom = identity
			opts.Destination = filepath.Join(t.TempDir(), "again")
			opts.Settings.LocalMachineName = "renamed-label"
			second, err := Capture(ctx, opts)
			require.NoError(t, err)
			assert.Equal(t, first.Source.DeviceID, second.Source.DeviceID)
			wantRoots := append([]RootSpec(nil), first.Source.Roots...)
			if condition == "application-directory-moved" {
				for i := range wantRoots {
					if wantRoots[i].ID == "application" {
						assert.Equal(t, original, wantRoots[i].OriginalPath)
						wantRoots[i].ConfiguredPath = opts.DataDir
					}
				}
				spec, err := LoadImportSpec(ctx, filepath.Join(opts.Destination, "capture.json"))
				require.NoError(t, err)
				_, err = archive.Import(ctx, spec)
				require.NoError(t, err, "moving application data preserves its accepted root identity")
			}
			assert.Equal(t, wantRoots, second.Source.Roots)
			assert.Empty(t, second.NewIdentities)
			assert.Equal(t, "renamed-label", second.Source.Machine)
			if condition == "unchanged" {
				dbtest.WriteTestFile(t, filepath.Join(opts.DataDir, "telemetry-install-id"), []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
				opts.Destination = filepath.Join(t.TempDir(), "wrong-installation")
				_, err = Capture(ctx, opts)
				require.ErrorContains(t, err, "different installation")
				assert.NoDirExists(t, opts.Destination)
			}
		})
	}
}

func TestCaptureOmitsProviderCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, provider, dir string
		omitIDE             bool
	}{
		{"claude-home", "claude", "projects", true},
		{"claude-project", "claude", "projects/project-a", false},
		{"codex-home", "codex", "sessions", false},
		{"supplemental", "files", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := captureFixture(t)
			root := filepath.Dir(opts.Roots[0].Path)
			selected := filepath.Join(root, filepath.FromSlash(tc.dir))
			transcript := "projects/project-b/ide/saved.jsonl"
			require.NoError(t, os.MkdirAll(selected, 0o700))
			opts.Roots = []RootSpec{{Provider: tc.provider, Path: selected}}
			if tc.name == "claude-project" {
				root = selected
				transcript = "nested/ide/saved.jsonl"
			}
			dbtest.WriteTestFile(t, filepath.Join(root, ".git-credentials"), []byte("synthetic credential"))
			dbtest.WriteTestFile(t, filepath.Join(root, "ide", "51234.lock"), []byte(`{"authToken":"synthetic-token"}`))
			dbtest.WriteTestFile(t, filepath.Join(root, "file-history", "version.lock"), []byte("retained companion"))
			dbtest.WriteTestFile(t, filepath.Join(root, filepath.FromSlash(transcript)), []byte("retained transcript"))
			descriptor, err := Capture(t.Context(), opts)
			require.NoError(t, err)
			captured := filepath.Join(opts.Destination, descriptor.Source.Roots[0].Path)
			assert.NoFileExists(t, filepath.Join(captured, ".git-credentials"))
			assert.Contains(t, descriptor.Omissions, CaptureOmission{
				RootID: descriptor.Source.Roots[0].ID, Path: ".git-credentials", Reason: "credential or runtime settings file",
			})
			if tc.omitIDE {
				assert.NoDirExists(t, filepath.Join(captured, "ide"))
				assert.Contains(t, descriptor.Omissions, CaptureOmission{
					RootID: descriptor.Source.Roots[0].ID, Path: "ide", Reason: "Claude IDE runtime state",
				})
			} else {
				assert.FileExists(t, filepath.Join(captured, "ide", "51234.lock"))
			}
			for path, want := range map[string]string{
				"file-history/version.lock": "retained companion",
				transcript:                  "retained transcript",
			} {
				body, err := os.ReadFile(filepath.Join(captured, filepath.FromSlash(path)))
				require.NoError(t, err)
				assert.Equal(t, want, string(body))
			}
		})
	}
}

func TestCaptureIdentityRejectsInvalidDescriptor(t *testing.T) {
	opts := captureFixture(t)
	first, err := Capture(t.Context(), opts)
	require.NoError(t, err)
	for _, fault := range []string{"version", "incomplete", "identity", "duplicate-root", "root-path", "session-path", "application-origin", "provider", "oversize"} {
		t.Run(fault, func(t *testing.T) {
			d := first
			d.Source.Roots = append([]RootSpec(nil), first.Source.Roots...)
			switch fault {
			case "version":
				d.Version++
			case "incomplete":
				d.CompletedAt = d.StartedAt.Add(-1)
			case "identity":
				d.Source.DeviceID = "invalid"
			case "duplicate-root":
				d.Source.Roots = append(d.Source.Roots, d.Source.Roots[0])
			case "root-path":
				d.Source.Roots[0].Path = "../payload"
			case "session-path":
				d.Source.Roots[0].SessionDirs = []string{"../payload"}
			case "application-origin":
				d.Source.Roots[1].OriginalPath = ""
			case "provider":
				d.Source.Roots[0].Provider = "invalid"
			}
			data, err := json.Marshal(d)
			require.NoError(t, err)
			identity := filepath.Join(t.TempDir(), "capture.json")
			require.NoError(t, os.WriteFile(identity, data, 0o600))
			if fault == "oversize" {
				require.NoError(t, os.Truncate(identity, (4<<20)+1))
			}
			next := opts
			next.IdentityFrom = identity
			next.Destination = filepath.Join(t.TempDir(), "next")
			_, err = Capture(t.Context(), next)
			require.Error(t, err)
			assert.NoDirExists(t, next.Destination)
		})
	}
}

func TestCapturePreservesRawSyncRoot(t *testing.T) {
	opts := captureFixture(t)
	path := filepath.Join(opts.DataDir, "raw-sync", "checkpoint.db")
	store, err := rawcheckpoint.Open(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, store.SetDevice(t.Context(), "hosted-device"))
	root, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, opts.Roots[0].Path)
	require.NoError(t, err)
	defer store.Close()
	descriptor, err := Capture(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, root.ID, descriptor.Source.Roots[0].ID)
	require.NotNil(t, descriptor.RawSync)
	assert.Equal(t, "hosted-device", descriptor.RawSync.DeviceID)
	assert.Equal(t, root.LocalPath, descriptor.RawSync.Roots[0].LocalPath)
	assert.NoFileExists(t, filepath.Join(opts.Destination, "roots", "application", "raw-sync", "checkpoint.db"))
}

func TestCaptureRejectsIncompletePackage(t *testing.T) {
	for _, fault := range []string{"missing", "changed", "extra", "version", "report"} {
		t.Run(fault, func(t *testing.T) {
			opts := captureFixture(t)
			d, err := Capture(t.Context(), opts)
			require.NoError(t, err)
			descriptor := filepath.Join(opts.Destination, "capture.json")
			transcript := filepath.Join(opts.Destination, d.Source.Roots[0].Path, "projects", "project-a", "saved.jsonl")
			switch fault {
			case "missing":
				require.NoError(t, os.Remove(transcript))
			case "changed":
				require.NoError(t, os.WriteFile(transcript, []byte("changed\n"), 0o600))
			case "extra":
				dbtest.WriteTestFile(t, filepath.Join(opts.Destination, d.Source.Roots[0].Path, "extra"), []byte("not inventoried"))
			case "version":
				d.Version = 2
			case "report":
				d.WritersStopped = !d.WritersStopped
			}
			if fault == "version" || fault == "report" {
				data, err := json.Marshal(d)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(descriptor, data, 0o600))
			}
			_, err = LoadImportSpec(t.Context(), descriptor)
			require.Error(t, err)
		})
	}
}

func TestCapturePreflightBlocksUnknownAndDeletedProjection(t *testing.T) {
	for _, condition := range []string{"absent", "trashed"} {
		t.Run(condition, func(t *testing.T) {
			opts := captureFixture(t)
			if condition == "absent" {
				require.NoError(t, os.Remove(filepath.Join(opts.DataDir, "sessions.db")))
			} else {
				database, err := db.OpenIsolatedContext(t.Context(), filepath.Join(opts.DataDir, "sessions.db"))
				require.NoError(t, err)
				require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "trashed", Agent: "claude", Project: "project-a", Machine: "source"}))
				require.NoError(t, database.SoftDeleteSession(t.Context(), "trashed"))
				require.NoError(t, database.Close())
			}
			d, err := Capture(t.Context(), opts)
			require.NoError(t, err)
			if condition == "absent" {
				assert.Nil(t, d.Preflight.Counts["trashed"])
				assert.NotEmpty(t, d.Preflight.Unknown)
			} else {
				require.NotNil(t, d.Preflight.Counts["trashed"])
				assert.EqualValues(t, 1, *d.Preflight.Counts["trashed"])
			}
			_, err = LoadImportSpec(t.Context(), filepath.Join(opts.Destination, "capture.json"))
			require.Error(t, err)
		})
	}
}

func TestCaptureDoesNotPublishFailure(t *testing.T) {
	for _, condition := range []string{"nested", "existing", "vault", "raw-vault", "canceled", "symlink-root", "invalid-identity", "missing-asset", "case-alias-overlap", "newer-checkpoint"} {
		t.Run(condition, func(t *testing.T) {
			opts := captureFixture(t)
			ctx := t.Context()
			switch condition {
			case "case-alias-overlap":
				parent := t.TempDir()
				provider := filepath.Join(parent, "Provider")
				require.NoError(t, os.Mkdir(provider, 0o755))
				alias := filepath.Join(parent, "provider")
				aliasInfo, err := os.Stat(alias)
				if err != nil {
					t.Skip("test filesystem is case-sensitive")
				}
				providerInfo, err := os.Stat(provider)
				require.NoError(t, err)
				if !os.SameFile(aliasInfo, providerInfo) {
					t.Skip("test filesystem does not resolve case aliases")
				}
				opts.Roots = []RootSpec{{Provider: "files", Path: provider}}
				opts.Destination = filepath.Join(alias, "capture")
			case "newer-checkpoint":
				path := filepath.Join(opts.DataDir, "raw-sync", "checkpoint.db")
				store, err := rawcheckpoint.Open(t.Context(), path)
				require.NoError(t, err)
				require.NoError(t, store.Close())
				checkpoint, err := sql.Open("sqlite3", path)
				require.NoError(t, err)
				_, err = checkpoint.ExecContext(t.Context(), "PRAGMA user_version=1000")
				require.NoError(t, err)
				require.NoError(t, checkpoint.Close())
			case "nested":
				opts.Destination = filepath.Join(opts.Roots[0].Path, "capture")
			case "existing":
				require.NoError(t, os.Mkdir(opts.Destination, 0o700))
				dbtest.WriteTestFile(t, filepath.Join(opts.Destination, "keep"), []byte("keep"))
			case "vault":
				require.NoError(t, os.Mkdir(filepath.Join(opts.DataDir, "artifacts"), 0o700))
			case "raw-vault":
				require.NoError(t, os.Mkdir(filepath.Join(opts.DataDir, Directory), 0o700))
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "symlink-root":
				alias := filepath.Join(t.TempDir(), "alias")
				require.NoError(t, os.Symlink(opts.Roots[0].Path, alias))
				opts.Roots[0].Path = alias
			case "invalid-identity":
				dbtest.WriteTestFile(t, filepath.Join(opts.DataDir, "telemetry-install-id"), nil)
			case "missing-asset":
				database, err := db.OpenIsolatedContext(ctx, filepath.Join(opts.DataDir, "sessions.db"))
				require.NoError(t, err)
				require.NoError(t, database.UpsertSession(ctx, db.Session{ID: "image", Agent: "claude", Machine: "source", Project: "project-a"}))
				require.NoError(t, database.InsertMessages(ctx, []db.Message{{SessionID: "image", Ordinal: 0, Role: "user", Content: "![image](asset://" + strings.Repeat("a", 64) + ".png)"}}))
				require.NoError(t, database.Close())
			}
			_, err := Capture(ctx, opts)
			require.Error(t, err)
			switch condition {
			case "case-alias-overlap":
				require.ErrorContains(t, err, "capture roots must not overlap")
				entries, err := os.ReadDir(opts.Roots[0].Path)
				require.NoError(t, err)
				assert.Empty(t, entries, "rejection must precede staging and copying")
			case "newer-checkpoint":
				require.ErrorContains(t, err, "newer than supported")
			}
			if condition == "existing" {
				assert.FileExists(t, filepath.Join(opts.Destination, "keep"))
			} else {
				assert.NoDirExists(t, opts.Destination)
			}
		})
	}
}

func TestCaptureRejectsChangedFile(t *testing.T) {
	source := filepath.Join(t.TempDir(), "transcript.jsonl")
	dbtest.WriteTestFile(t, source, []byte("before"))
	info, err := os.Stat(source)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(source, []byte("changed after enumeration"), 0o600))
	target := filepath.Join(t.TempDir(), "copy")
	_, err = captureFile(t.Context(), source, target, "source", "transcript.jsonl", info)
	require.ErrorContains(t, err, "source changed")
	assert.NoFileExists(t, target)
}

func TestCaptureRejectsDuplicateSelectedRoots(t *testing.T) {
	for _, identity := range []string{"new", "reused"} {
		t.Run(identity, func(t *testing.T) {
			opts := captureFixture(t)
			if identity == "reused" {
				_, err := Capture(t.Context(), opts)
				require.NoError(t, err)
				opts.IdentityFrom = filepath.Join(opts.Destination, "capture.json")
				opts.Destination = filepath.Join(t.TempDir(), "again")
			}
			alias := filepath.Join(t.TempDir(), "alias")
			require.NoError(t, os.Symlink(filepath.Dir(opts.Roots[0].Path), alias))
			for _, path := range []string{opts.Roots[0].Path, filepath.Join(alias, "projects")} {
				opts.Roots = []RootSpec{opts.Roots[0], {Provider: "claude", Path: path}}
				_, err := Capture(t.Context(), opts)
				require.EqualError(t, err, "duplicate selected capture root")
				assert.NoDirExists(t, opts.Destination)
			}
			for _, tc := range []struct {
				name, provider string
				paths          []string
				duplicate      bool
			}{
				{"claude-home-projects", "claude", []string{".", "projects"}, true},
				{"claude-projects-home", "claude", []string{"projects", "."}, true},
				{"claude-projects-nested", "claude", []string{"projects", "projects/project-a"}, true},
				{"claude-nested-projects", "claude", []string{"projects/project-a", "projects"}, true},
				{"codex-home-sessions", "codex", []string{".", "sessions"}, true},
				{"codex-sessions-home", "codex", []string{"sessions", "."}, true},
				{"codex-home-archived", "codex", []string{".", "archived_sessions"}, true},
				{"codex-disjoint", "codex", []string{"sessions", "archived_sessions"}, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					opts := captureFixture(t)
					root := filepath.Dir(opts.Roots[0].Path)
					if tc.provider == "codex" {
						for _, dir := range []string{"sessions", "archived_sessions"} {
							dbtest.WriteTestFile(t, filepath.Join(root, dir, "saved.jsonl"), []byte("original bytes\n"))
						}
					}
					opts.Roots = nil
					for _, path := range tc.paths {
						opts.Roots = append(opts.Roots, RootSpec{Provider: tc.provider, Path: filepath.Join(root, filepath.FromSlash(path))})
					}
					if identity == "reused" {
						first := opts
						first.Roots = opts.Roots[:1]
						_, err := Capture(t.Context(), first)
						require.NoError(t, err)
						opts.IdentityFrom = filepath.Join(first.Destination, "capture.json")
						opts.Destination = filepath.Join(t.TempDir(), "again")
					}
					descriptor, err := Capture(t.Context(), opts)
					if tc.duplicate {
						require.EqualError(t, err, "duplicate selected capture root")
						assert.NoDirExists(t, opts.Destination)
					} else {
						require.NoError(t, err)
						require.Len(t, descriptor.Source.Roots, 3)
						assert.NotEqual(t, descriptor.Source.Roots[0].ID, descriptor.Source.Roots[1].ID)
						assert.FileExists(t, filepath.Join(opts.Destination, descriptor.Source.Roots[0].Path, "sessions", "saved.jsonl"))
						assert.FileExists(t, filepath.Join(opts.Destination, descriptor.Source.Roots[1].Path, "archived_sessions", "saved.jsonl"))
					}
				})
			}
		})
	}
}
