// Package why inspects local Git provenance and assembles archived evidence.
package why

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const maxLines = 400
const maxCommits = 6

var shaPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
var rangePattern = regexp.MustCompile(`^([0-9]+)(?:-([0-9]+))?$`)

// Request describes one path/range or commit; zero range means the whole file.
type Request struct {
	Path        string
	Start, End  int
	Rev, Commit string
	LiteralPath bool
}

// Question is the canonical JSON question. Range pointers preserve empty-file zeros.
type Question struct {
	Path   string `json:"path,omitzero"`
	Start  *int   `json:"start,omitzero"`
	End    *int   `json:"end,omitzero"`
	Rev    string `json:"rev,omitzero"`
	Commit string `json:"commit,omitzero"`
}

// Line preserves the selected final line number and author metadata.
type Line struct {
	N          int     `json:"n"`
	Text       string  `json:"text"`
	SHA        *string `json:"sha"`
	At         string  `json:"at"`
	Author     string  `json:"author"`
	commitTime int64
}

// Commit is selected by line coverage, then committer time, then SHA.
type Commit struct {
	SHA, At, Author, Subject, Message string
	Covered                           int
	at                                int64
}

// ParseRequest validates mutually exclusive modes and the reserved numeric suffix.
func ParseRequest(path, commit, rev string, literal bool) (Request, error) {
	r := Request{Path: path, Commit: strings.ToLower(commit), Rev: rev, LiteralPath: literal}
	if (path == "") == (commit == "") {
		return r, errors.New("supply one path or --commit SHA")
	}
	if !utf8.ValidString(path) || strings.ContainsRune(path, 0) {
		return r, errors.New("path must be valid UTF-8 without NUL")
	}
	if commit != "" {
		if !shaPattern.MatchString(commit) || rev != "" || literal {
			return r, errors.New("--commit requires a 7–40 character hexadecimal SHA and cannot be combined with --rev or --literal-path")
		}
		return r, nil
	}
	if literal {
		return r, nil
	}
	if i := strings.LastIndexByte(path, ':'); i >= 0 && i+1 < len(path) && path[i+1] >= '0' && path[i+1] <= '9' {
		m := rangePattern.FindStringSubmatch(path[i+1:])
		if m == nil {
			return r, errors.New("invalid line range; use path:start-end or --literal-path")
		}
		start, err := strconv.Atoi(m[1])
		if err != nil {
			return r, errors.New("line range overflows an integer")
		}
		end := start
		if m[2] != "" {
			end, err = strconv.Atoi(m[2])
		}
		if err != nil || start < 1 || end < start || end-start >= maxLines {
			return r, fmt.Errorf("line range must be positive, ordered, and at most %d lines", maxLines)
		}
		r.Path = path[:i]
		r.Start = start
		r.End = end
		if r.Path == "" {
			return r, errors.New("path is empty")
		}
	}
	return r, nil
}

func parseBlame(raw []byte) ([]Line, error) {
	lines := []Line{}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	// Porcelain content is LF-framed; a CR before LF belongs to the source.
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	var current *Line
	var authorTime, committerTime bool
	for scanner.Scan() {
		text := scanner.Text()
		if current == nil {
			f := strings.Fields(text)
			if len(f) < 3 || len(f) > 4 || len(f[0]) != 40 || !shaPattern.MatchString(f[0]) {
				return nil, errors.New("malformed git blame header")
			}
			if len(f) == 4 {
				count, err := strconv.Atoi(f[3])
				if err != nil || count < 1 {
					return nil, errors.New("malformed git blame group count")
				}
			}
			original, e1 := strconv.Atoi(f[1])
			n, e2 := strconv.Atoi(f[2])
			if e1 != nil || e2 != nil || original < 1 || n < 1 {
				return nil, errors.New("malformed git blame line number")
			}
			if len(lines) > 0 && n != lines[len(lines)-1].N+1 {
				return nil, errors.New("noncontiguous git blame lines")
			}
			sha := strings.ToLower(f[0])
			current = &Line{N: n, SHA: &sha}
			if sha == strings.Repeat("0", 40) {
				current.SHA = nil
			}
			authorTime = false
			committerTime = false
			continue
		}
		if strings.HasPrefix(text, "\t") {
			if !authorTime || !committerTime {
				return nil, errors.New("git blame is missing timestamps")
			}
			current.Text = strings.ToValidUTF8(text[1:], "\uFFFD")
			if current.SHA == nil {
				current.At = ""
				current.Author = ""
			}
			lines = append(lines, *current)
			if len(lines) > maxLines {
				return nil, fmt.Errorf("more than %d lines; supply a smaller path:start-end range", maxLines)
			}
			current = nil
			continue
		}
		key, value, _ := strings.Cut(text, " ")
		switch key {
		case "author":
			current.Author = strings.ToValidUTF8(value, "\uFFFD")
		case "author-time", "committer-time":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < -62135596800 || n > 253402300799 {
				return nil, errors.New("malformed git blame timestamp")
			}
			if key == "author-time" {
				current.At = time.Unix(n, 0).UTC().Format(time.RFC3339)
				authorTime = true
			} else {
				current.commitTime = n
				committerTime = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("git blame line exceeds limit: %w", err)
	}
	if current != nil {
		return nil, errors.New("incomplete git blame record")
	}
	return lines, nil
}
func selectCommits(lines []Line) ([]Commit, int) {
	bySHA := map[string]*Commit{}
	for _, line := range lines {
		if line.SHA == nil {
			continue
		}
		c := bySHA[*line.SHA]
		if c == nil {
			c = &Commit{SHA: *line.SHA, at: line.commitTime}
			bySHA[c.SHA] = c
		}
		c.Covered++
	}
	out := make([]Commit, 0, len(bySHA))
	for _, c := range bySHA {
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b Commit) int {
		if a.Covered != b.Covered {
			return b.Covered - a.Covered
		}
		if a.at > b.at {
			return -1
		}
		if a.at < b.at {
			return 1
		}
		return strings.Compare(a.SHA, b.SHA)
	})
	older := 0
	if len(out) > maxCommits {
		for _, c := range out[maxCommits:] {
			older += c.Covered
		}
		out = out[:maxCommits]
	}
	return out, older
}
