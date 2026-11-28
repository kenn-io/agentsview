package parser

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cbcParentTranscript writes a one-message transcript under project whose
// filename stem differs from the session id it records, so a parent link
// that names the stem proves the transcript was found.
func cbcParentTranscript(t *testing.T, project, stem, runtimeID string) string {
	t.Helper()
	path := filepath.Join(project, stem+".jsonl")
	writeCBC(t, path,
		`{"id":"u1","type":"message","role":"user","timestamp":900,"sessionId":"`+runtimeID+`","content":[{"type":"input_text","text":"first"}]}`)
	return path
}

// cbcForkBranch writes the branch transcript run-b and a catalog in which
// conversation c-b forked from c-parent, plus statements describing the
// parent's runtime ids.
func cbcForkBranch(t *testing.T, home, project string, parent ...string) string {
	t.Helper()
	branch := filepath.Join(project, "run-b.jsonl")
	writeCBC(t, branch,
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"sessionId":"run-b","content":[{"type":"input_text","text":"branch"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"ok"}]}`,
	)
	writeCBCCatalog(t, home, append(parent,
		`INSERT INTO conversations (id, title, runtime_session_id, fork_source_id) VALUES ('c-b', 'Branch', 'run-b', 'c-parent')`)...)
	return branch
}

// A conversation can have an explicit runtime session id and local runtime
// mappings, and only some of them may have a transcript on disk. A fork links
// to the first of them, explicit id first and then mappings in sorted order,
// whose transcript exists, whatever order the mapping rows were written in.
func TestCodeBuddyCBCForkParentUsesAvailableRuntimeAlias(t *testing.T) {
	const explicitParent = `INSERT INTO conversations (id, title, runtime_session_id) VALUES ('c-parent', 'Parent', 'run-p')`
	const implicitParent = `INSERT INTO conversations (id, title) VALUES ('c-parent', 'Parent')`
	mapping := func(runtime string) string {
		return `INSERT INTO conversation_runtime_sources VALUES ('', '` + runtime + `', 'c-parent')`
	}
	for _, tc := range []struct {
		name        string
		catalog     []string
		transcripts map[string]string // filename stem -> recorded session id
		want        string
	}{
		{
			name:        "explicit transcript beside a local mapping",
			catalog:     []string{explicitParent, mapping("run-q")},
			transcripts: map[string]string{"parent-file": "run-p"},
			want:        "codebuddy:parent-file",
		},
		{
			name:        "explicit transcript beside two mappings",
			catalog:     []string{explicitParent, mapping("run-q1"), mapping("run-q2")},
			transcripts: map[string]string{"parent-file": "run-p"},
			want:        "codebuddy:parent-file",
		},
		{
			name:        "explicit transcript beside two mappings written in reverse",
			catalog:     []string{explicitParent, mapping("run-q2"), mapping("run-q1")},
			transcripts: map[string]string{"parent-file": "run-p"},
			want:        "codebuddy:parent-file",
		},
		{
			name:        "only the mapped transcript exists",
			catalog:     []string{explicitParent, mapping("run-q")},
			transcripts: map[string]string{"mapped-file": "run-q"},
			want:        "codebuddy:mapped-file",
		},
		{
			name:    "mappings without an explicit id",
			catalog: []string{implicitParent, mapping("run-q1"), mapping("run-q2")},
			transcripts: map[string]string{
				"file-q1": "run-q1", "file-q2": "run-q2",
			},
			want: "codebuddy:file-q1",
		},
		{
			name:    "mappings without an explicit id written in reverse",
			catalog: []string{implicitParent, mapping("run-q2"), mapping("run-q1")},
			transcripts: map[string]string{
				"file-q1": "run-q1", "file-q2": "run-q2",
			},
			want: "codebuddy:file-q1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, project := cbcHome(t)
			for stem, runtimeID := range tc.transcripts {
				cbcParentTranscript(t, project, stem, runtimeID)
			}
			cbcForkBranch(t, home, project, tc.catalog...)
			branch := cbcSession(t, parseCBCHome(t, home), "codebuddy:run-b")
			assert.Equal(t, tc.want, branch.Session.ParentSessionID)
			assert.Equal(t, RelFork, branch.Session.RelationshipType)
			for stem := range tc.transcripts {
				assert.Equal(t, "Parent",
					cbcSession(t, parseCBCHome(t, home), "codebuddy:"+stem).Session.SessionName,
					"every runtime id of the parent reads its catalog row")
			}
		})
	}
}

// The quick-sync cutoff of a branch counts the parent's transcript under any
// of the parent's runtime ids, so a parent arriving under one of them makes
// the branch look recent and the next quick sync repairs its link.
func TestCodeBuddyCutoffCountsEveryForkParentRuntimeAlias(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded string
	}{
		{"explicit runtime id", "run-p"},
		{"mapped runtime id", "run-q"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, project := cbcHome(t)
			branch := cbcForkBranch(t, home, project,
				`INSERT INTO conversations (id, title, runtime_session_id) VALUES ('c-parent', 'Parent', 'run-p')`,
				`INSERT INTO conversation_runtime_sources VALUES ('', 'run-q', 'c-parent')`)
			parent := cbcParentTranscript(t, project, "parent-file", tc.recorded)
			future := time.Now().Add(time.Hour)
			require.NoError(t, os.Chtimes(parent, future, future))

			cutoff, err := CodeBuddyCutoffMtime(t.Context(), branch,
				[]string{filepath.Dir(project)})
			require.NoError(t, err)
			assert.GreaterOrEqual(t, cutoff, future.UnixNano(),
				"the parent's transcript counts toward the branch cutoff")
		})
	}
}
