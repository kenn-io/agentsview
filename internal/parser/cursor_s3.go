package parser

import (
	"path"
	"strings"
)

func (p *cursorProvider) S3Scanner() S3SessionScanner {
	return cursorS3Scanner()
}

func cursorS3Scanner() S3SessionScanner {
	return S3SessionScanner{
		Agent:   AgentCursor,
		Keep:    keepCursorS3Session,
		Project: cursorS3Project,
	}
}

func cursorS3Project(_ string, segs []string) string {
	if len(segs) == 0 {
		return ""
	}
	if len(segs) >= 3 && segs[1] == "agent-transcripts" {
		project := DecodeCursorProjectDir(segs[0])
		if project == "" {
			return "unknown"
		}
		return project
	}
	return segs[0]
}

func discoverCursorS3(root string) []DiscoveredFile {
	return preferCursorS3Transcripts(root, s3PrefixScan(root, cursorS3Scanner()))
}

// keepCursorS3Session accepts the documented harvest layout
// <project>/<id>.{jsonl,txt} and the local Cursor layouts under
// <project>/agent-transcripts/. Other .jsonl/.txt objects are ignored.
func keepCursorS3Session(_ string, segs []string) bool {
	switch len(segs) {
	case 2:
		return cursorS3TranscriptName(segs[1])
	case 3:
		return segs[1] == "agent-transcripts" && cursorS3TranscriptName(segs[2])
	case 4:
		if segs[1] != "agent-transcripts" || !IsCursorTranscriptExt(segs[3]) {
			return false
		}
		stem := strings.TrimSuffix(segs[3], path.Ext(segs[3]))
		return stem == segs[2] && IsValidSessionID(stem)
	default:
		return false
	}
}

func cursorS3TranscriptName(name string) bool {
	if !IsCursorTranscriptExt(name) {
		return false
	}
	stem := strings.TrimSuffix(name, path.Ext(name))
	return IsValidSessionID(stem)
}

// preferCursorS3Transcripts keeps one object per encoded project directory
// and stem. Precedence matches local Cursor discovery: .jsonl over .txt,
// then nested <id>/<id>.ext over flat <id>.ext, then lexical path.
func preferCursorS3Transcripts(root string, files []DiscoveredFile) []DiscoveredFile {
	type key struct {
		project string
		stem    string
	}
	best := make(map[key]DiscoveredFile, len(files))
	order := make([]key, 0, len(files))
	for _, file := range files {
		k := key{
			project: cursorS3EncodedProject(root, file.Path),
			stem:    strings.TrimSuffix(path.Base(file.Path), path.Ext(file.Path)),
		}
		prev, ok := best[k]
		if !ok {
			best[k] = file
			order = append(order, k)
			continue
		}
		if cursorS3Prefers(file, prev) {
			best[k] = file
		}
	}
	out := make([]DiscoveredFile, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}

func cursorS3EncodedProject(root, uri string) string {
	rel, ok := s3RelativePath(root, uri)
	if !ok {
		return ""
	}
	if before, _, ok := strings.Cut(rel, "/"); ok {
		return before
	}
	return rel
}

func cursorS3Prefers(candidate, current DiscoveredFile) bool {
	candJSONL := strings.HasSuffix(candidate.Path, ".jsonl")
	currJSONL := strings.HasSuffix(current.Path, ".jsonl")
	if candJSONL != currJSONL {
		return candJSONL
	}
	candNested := cursorS3NestedTranscript(candidate.Path)
	currNested := cursorS3NestedTranscript(current.Path)
	if candNested != currNested {
		return candNested
	}
	return candidate.Path < current.Path
}

func cursorS3NestedTranscript(uri string) bool {
	stem := strings.TrimSuffix(path.Base(uri), path.Ext(uri))
	return path.Base(path.Dir(uri)) == stem
}
