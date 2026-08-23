package parser

import (
	"context"
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

type cursorS3Transcript struct {
	root string
	file DiscoveredFile
}

func discoverCursorS3ByRoot(
	ctx context.Context, roots []string,
) (map[string][]DiscoveredFile, error) {
	var candidates []cursorS3Transcript
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !isS3URI(root) {
			continue
		}
		for _, file := range s3PrefixScan(root, cursorS3Scanner()) {
			candidates = append(candidates, cursorS3Transcript{
				root: root,
				file: file,
			})
		}
	}

	byRoot := make(map[string][]DiscoveredFile)
	for _, transcript := range preferCursorS3Transcripts(candidates) {
		byRoot[transcript.root] = append(
			byRoot[transcript.root], transcript.file,
		)
	}
	return byRoot, nil
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

// preferCursorS3Transcripts keeps one object per machine and session stem
// across all configured S3 roots. Precedence matches local Cursor discovery:
// .jsonl over .txt, then nested <id>/<id>.ext over flat <id>.ext, then lexical
// path.
func preferCursorS3Transcripts(
	transcripts []cursorS3Transcript,
) []cursorS3Transcript {
	type key struct {
		machine string
		stem    string
	}
	best := make(map[key]cursorS3Transcript, len(transcripts))
	order := make([]key, 0, len(transcripts))
	for _, transcript := range transcripts {
		file := transcript.file
		k := key{
			machine: file.Machine,
			stem:    strings.TrimSuffix(path.Base(file.Path), path.Ext(file.Path)),
		}
		prev, ok := best[k]
		if !ok {
			best[k] = transcript
			order = append(order, k)
			continue
		}
		if cursorS3Prefers(file, prev.file) {
			best[k] = transcript
		}
	}
	out := make([]cursorS3Transcript, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
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
