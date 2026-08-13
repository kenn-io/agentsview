package signals

// Incremental signal maintenance: compact per-session aggregate state that
// lets the sync engine fold an incremental delta (appended tool calls plus
// late tool-result updates) into the same signal values a full recompute
// would produce, without loading session history.
//
// Correctness contract: every detector whose input can be changed by a
// delta is maintained exactly, as long as
//
//   - appended calls arrive in chronological order, and
//   - every modified (late-updated) call sits within the trailing
//     ModifiedWindowSize calls of the pre-delta session.
//
// The caller must fall back to a full recompute (and reseed) when a delta
// touches a call older than ModifiedWindowSize, when the delta contains
// user prompts or compact boundaries (not maintained here — see the sync
// maintainer), or when the persisted state is missing/stale.

import (
	"encoding/json"
	"fmt"
	"slices"
)

const (
	// IncrementalStateCodecVersion is the wire version for IncrementalState.
	// Bump when the struct or any detector semantics change; a mismatch
	// makes the caller fall back to a full recompute.
	IncrementalStateCodecVersion = 1

	// TrailingFactCount is the size of the trailing facts window. It must
	// cover every window any delta can affect: a modified call in the last
	// ModifiedWindowSize calls can touch runaway windows that start up to
	// 11 calls earlier, so the window must exceed ModifiedWindowSize + 11.
	TrailingFactCount = 35

	// ModifiedWindowSize is the number of trailing calls whose facts a
	// late tool-result update may change on the incremental path. Updates
	// to older calls require a full recompute.
	ModifiedWindowSize = 12
)

// CallPos identifies one tool call by its natural message/call coordinates.
type CallPos struct {
	MessageOrdinal int `json:"message_ordinal"`
	CallIndex      int `json:"call_index"`
}

// ToolFact is the per-call fact set the incremental machinery needs: the
// position plus the failure bit, exact tool signature, and command class.
type ToolFact struct {
	CallPos
	Failure        bool   `json:"failure"`
	ExactSignature string `json:"exact_signature,omitempty"`
	CommandClass   string `json:"command_class,omitempty"`
}

// EditChurnState tracks one file path's churn detection: the last two edit
// ordinals plus a counted latch (churn counts once per file).
type EditChurnState struct {
	Last1   int  `json:"last1"`
	Last2   int  `json:"last2"`
	Counted bool `json:"counted"`
}

// PendingBoundary tracks a compact boundary whose after-window is still
// open: fewer than midTaskWindowAfter calls have arrived since it, and the
// overlap threshold has not been met. BeforeNames is frozen at seed time
// (appends are chronological, so the pre-boundary window never changes).
type PendingBoundary struct {
	Ordinal     int      `json:"ordinal"`
	BeforeNames []string `json:"before_names,omitempty"`
	AfterNames  []string `json:"after_names,omitempty"`
}

// IncrementalState is the SQLite-only compact aggregate state for one
// session. It is JSON-encoded and versioned; the sync layer additionally
// stamps a verification token (transcript revision + signal version) next
// to it so a state that fell behind the rows is never folded.
type IncrementalState struct {
	CodecVersion int `json:"codec_version"`

	// Failure runs. PrefixFailureMax is the longest failure run ending
	// before the trailing window; TailFailureRun is the length of the
	// failure run ending at the last call before the window (the run that
	// crosses into the window when window[0] is a failure).
	PrefixFailureMax int `json:"prefix_failure_max"`
	TailFailureRun   int `json:"tail_failure_run"`

	// Retry run tail: the trailing run of calls sharing (ToolName,
	// InputJSON). RetryCount itself lives on the sessions row.
	RetryRunName  string `json:"retry_run_name,omitempty"`
	RetryRunInput string `json:"retry_run_input,omitempty"`
	RetryRunLen   int    `json:"retry_run_len"`

	// Edit churn: the last two edit ordinals per file path plus the
	// counted latch. EditChurnCount itself lives on the sessions row.
	EditLast map[string]EditChurnState `json:"edit_last,omitempty"`

	// Runaway loop: RunawayHistorical latches hasRunawayToolWindow over
	// every 12-window that has fully left the trailing facts window.
	RunawayHistorical bool `json:"runaway_historical"`

	// Exact failing run crossing into the trailing window. ExactRunSig is
	// empty when the run containing window[0] starts inside the window.
	// ExactHistorical latches qualifying runs fully before the window.
	ExactRunSig      string `json:"exact_run_sig,omitempty"`
	ExactRunLen      int    `json:"exact_run_len"`
	ExactRunFailures int    `json:"exact_run_failures"`
	ExactHistorical  bool   `json:"exact_historical"`

	// Message-derived aggregates not stored on the sessions row.
	LastRole        string         `json:"last_role,omitempty"`
	LastContent     string         `json:"last_content,omitempty"`
	MsgIndex        int            `json:"msg_index"`
	LastValidTokens int            `json:"last_valid_tokens"`
	ModelCounts     map[string]int `json:"model_counts,omitempty"`
	ModelFirstSeen  map[string]int `json:"model_first_seen,omitempty"`

	// TotalCalls counts calls folded so far; used for window arithmetic.
	TotalCalls int `json:"total_calls"`

	// PendingBoundaries are compact boundaries with an open after-window.
	PendingBoundaries []PendingBoundary `json:"pending_boundaries,omitempty"`

	// Trailing holds the facts of the last TrailingFactCount calls in
	// chronological order.
	Trailing []ToolFact `json:"trailing,omitempty"`
}

// ToolHealthRow is the subset of the sessions row the tool-health fold
// combines with deltas: the current stored aggregates.
type ToolHealthRow struct {
	FailureCount   int
	RetryCount     int
	EditChurnCount int
}

// ToolHealthResult carries the absolute tool-health signal values after a
// fold. MidTaskCompactions is the number of pending boundaries the delta
// newly classified as mid-task (a delta against the stored row value).
type ToolHealthResult struct {
	FailureCount          int
	ConsecutiveFailureMax int
	RetryCount            int
	EditChurnCount        int
	RunawayToolLoopCount  int
	FinalFailureStreak    int
	MidTaskCompactions    int
}

// SeedIncrementalState builds the initial state from a full compute's
// inputs. calls must be ordered by (MessageOrdinal, CallIndex) — the same
// order extractToolCallRows produces. boundaries must be ascending
// compact-boundary ordinals. modelCounts/modelFirstSeen/msgIndex mirror
// extractMostCommonModel's inputs; lastValidTokens is the last assistant
// context-token measurement (0 when none).
func SeedIncrementalState(
	calls []ToolCallRow,
	boundaries []int,
	lastRole, lastContent string,
	modelCounts, modelFirstSeen map[string]int,
	msgIndex int,
	lastValidTokens int,
) IncrementalState {
	s := IncrementalState{
		CodecVersion:    IncrementalStateCodecVersion,
		EditLast:        map[string]EditChurnState{},
		LastRole:        lastRole,
		LastContent:     lastContent,
		LastValidTokens: lastValidTokens,
		MsgIndex:        msgIndex,
		ModelCounts:     modelCounts,
		ModelFirstSeen:  modelFirstSeen,
		TotalCalls:      len(calls),
	}
	cut := max(0, len(calls)-TrailingFactCount)
	s.Trailing = factsFor(calls[cut:])

	// Failure runs: latch runs ending before the cut, crossing run at it.
	// TailFailureRun holds the portion of the boundary run before the
	// window (positions < cut), so a fold can replay it as leading trues.
	for i := 0; i < len(calls); {
		if !IsFailure(calls[i]) {
			i++
			continue
		}
		j := i
		for j+1 < len(calls) && IsFailure(calls[j+1]) {
			j++
		}
		if j < cut {
			s.PrefixFailureMax = max(s.PrefixFailureMax, j-i+1)
		}
		if cut > 0 && i < cut && j >= cut-1 {
			s.TailFailureRun = cut - i
		}
		i = j + 1
	}

	// Retry tail: the run containing the last call.
	if n := len(calls); n > 0 {
		s.RetryRunName = calls[n-1].ToolName
		s.RetryRunInput = calls[n-1].InputJSON
		s.RetryRunLen = 1
		for i := n - 2; i >= 0; i-- {
			if calls[i].ToolName == s.RetryRunName &&
				calls[i].InputJSON == s.RetryRunInput {
				s.RetryRunLen++
			} else {
				break
			}
		}
	}

	// Edit churn: last two edit ordinals per file plus the counted latch.
	fileOrdinals := map[string][]int{}
	for _, c := range calls {
		if c.Category != "Edit" && c.Category != "Write" {
			continue
		}
		path := extractFilePath(c.InputJSON)
		if path == "" {
			continue
		}
		fileOrdinals[path] = append(fileOrdinals[path], c.MessageOrdinal)
	}
	for path, ords := range fileOrdinals {
		st := EditChurnState{Counted: hasChurnWindow(ords, 3, 10)}
		if n := len(ords); n >= 1 {
			st.Last2 = ords[n-1]
		}
		if n := len(ords); n >= 2 {
			st.Last1 = ords[n-2]
		}
		s.EditLast[path] = st
	}

	// Exact-run detector: seed via the same unified fold the incremental
	// path uses, over a deep prefix of nothing and the full call list.
	deep := deepRun{}
	historical, crossing, _ := foldExactRuns(
		deep, factsFor(calls), cut, false,
	)
	s.ExactHistorical = historical
	s.ExactRunSig = crossing.sig
	s.ExactRunLen = crossing.len
	s.ExactRunFailures = crossing.failures

	// Runaway window detector: latch every 12-window fully before the cut.
	for i := 0; i+12 <= cut; i++ {
		if windowFactsQualify(factsFor(calls[i : i+12])) {
			s.RunawayHistorical = true
		}
	}

	// Pending compaction boundaries: after-window still open and the
	// pre-boundary window non-empty (an empty before-window can never
	// reach the overlap threshold — mirror CountMidTaskCompactions).
	ords := callOrdinals(calls)
	for _, b := range boundaries {
		before := toolWindowBefore(ords, b, midTaskWindowBefore)
		if len(before) == 0 {
			continue
		}
		after := toolWindowAfter(ords, b, midTaskWindowAfter)
		if len(after) >= midTaskWindowAfter {
			continue // decided: below threshold forever
		}
		if overlapCount(before, after) >= midTaskOverlapThreshold {
			continue // already counted by the full compute
		}
		s.PendingBoundaries = append(s.PendingBoundaries, PendingBoundary{
			Ordinal:     b,
			BeforeNames: before,
			AfterNames:  after,
		})
	}
	return s
}

func factsFor(calls []ToolCallRow) []ToolFact {
	facts := make([]ToolFact, 0, len(calls))
	for _, c := range calls {
		facts = append(facts, ToolFact{
			CallPos: CallPos{
				MessageOrdinal: c.MessageOrdinal,
				CallIndex:      c.CallIndex,
			},
			Failure:        IsFailure(c),
			ExactSignature: ExactToolSignature(c),
			CommandClass:   CommandClass(c),
		})
	}
	return facts
}

// ExactToolSignature returns the exact signature the runaway exact-run
// detector uses for a call.
func ExactToolSignature(c ToolCallRow) string { return toolSignature(c) }

// CommandClass returns the command class the runaway window detector uses.
func CommandClass(c ToolCallRow) string { return commandClass(c) }

func callOrdinals(calls []ToolCallRow) []ToolCallOrdinal {
	ords := make([]ToolCallOrdinal, 0, len(calls))
	for _, c := range calls {
		ords = append(ords, ToolCallOrdinal{
			MessageOrdinal: c.MessageOrdinal,
			ToolName:       c.ToolName,
		})
	}
	return ords
}

func overlapCount(a, b []string) int {
	set := make(map[string]struct{}, len(a))
	for _, name := range a {
		set[name] = struct{}{}
	}
	matched := 0
	seen := make(map[string]struct{})
	for _, name := range b {
		if _, ok := set[name]; ok {
			if _, dup := seen[name]; !dup {
				seen[name] = struct{}{}
				matched++
			}
		}
	}
	return matched
}

// FoldToolHealth folds one incremental delta into the state. appended are
// the newly appended calls in chronological order; modified carries the
// post-delta facts of calls whose facts changed via late results (their
// pre-delta facts must be in s.Trailing); row supplies the stored
// aggregates the deltas combine with. It returns the next state and the
// absolute tool-health values, or ok=false when a modified position falls
// outside the maintenance window and the caller must fall back to a full
// recompute.
func (s *IncrementalState) FoldToolHealth(
	appended []ToolCallRow,
	modified map[CallPos]ToolFact,
	row ToolHealthRow,
) (IncrementalState, ToolHealthResult, bool) {
	if s.CodecVersion != IncrementalStateCodecVersion {
		return *s, ToolHealthResult{}, false
	}
	next := *s
	next.Trailing = nil // rebuilt below
	next.TotalCalls = s.TotalCalls + len(appended)

	// Every modified position must sit within the last ModifiedWindowSize
	// calls of the pre-delta session, and its old fact must be known.
	oldLimit := max(0, len(s.Trailing)-ModifiedWindowSize)
	oldByPos := make(map[CallPos]ToolFact, len(s.Trailing))
	for _, f := range s.Trailing {
		oldByPos[f.CallPos] = f
	}
	for pos := range modified {
		oldFact, inOld := oldByPos[pos]
		if !inOld {
			return *s, ToolHealthResult{}, false
		}
		idx := slices.IndexFunc(s.Trailing, func(f ToolFact) bool {
			return f.CallPos == pos
		})
		if idx < 0 || idx < oldLimit {
			return *s, ToolHealthResult{}, false
		}
		_ = oldFact
	}

	// Full tail facts: old window + appended, then overlay modifications.
	fullTail := mergeFacts(s.Trailing, factsFor(appended))
	for pos, newFact := range modified {
		idx := slices.IndexFunc(fullTail, func(f ToolFact) bool {
			return f.CallPos == pos
		})
		if idx < 0 {
			return *s, ToolHealthResult{}, false
		}
		fullTail[idx] = newFact
	}
	newCutAbs := max(0, next.TotalCalls-TrailingFactCount)
	oldCut := max(0, s.TotalCalls-TrailingFactCount)
	// fullTail[0] sits at absolute position oldCut, so the window offset
	// is the difference between the two cuts.
	newCut := newCutAbs - oldCut
	if newCut < 0 {
		newCut = 0
	}
	if newCut > len(fullTail) {
		newCut = len(fullTail)
	}
	next.Trailing = append([]ToolFact(nil), fullTail[newCut:]...)

	// Failure count: flips plus appended failures.
	failureDelta := 0
	for _, f := range appended {
		if IsFailure(f) {
			failureDelta++
		}
	}
	for pos, newFact := range modified {
		oldFact := oldByPos[pos]
		if newFact.Failure && !oldFact.Failure {
			failureDelta++
		} else if !newFact.Failure && oldFact.Failure {
			failureDelta--
		}
	}
	out := ToolHealthResult{
		FailureCount: row.FailureCount + failureDelta,
	}

	// Failure run structure: deep tail run + facts before the new cut +
	// new window bits.
	seqBefore := make([]bool, 0, s.TailFailureRun+newCut)
	for range s.TailFailureRun {
		seqBefore = append(seqBefore, true)
	}
	for _, f := range fullTail[:newCut] {
		seqBefore = append(seqBefore, f.Failure)
	}
	newBits := make([]bool, len(next.Trailing))
	for i, f := range next.Trailing {
		newBits[i] = f.Failure
	}
	prefixMax, tailRun, crossingRun := foldFailureRuns(
		s.PrefixFailureMax, seqBefore, newBits,
	)
	next.PrefixFailureMax = prefixMax
	next.TailFailureRun = tailRun
	// The boundary run itself is a real run: it counts toward the global
	// max even when the window starts with a non-failure (in which case
	// crossingRun is 0 and the run's full length is tailRun).
	out.ConsecutiveFailureMax = max(
		prefixMax, tailRun, crossingRun, maxRunWithin(newBits),
	)
	out.FinalFailureStreak = trailingRun(newBits)

	// Retry runs: only appends change name/input runs.
	out.RetryCount = row.RetryCount
	for _, c := range appended {
		if next.RetryRunLen > 0 &&
			c.ToolName == next.RetryRunName &&
			c.InputJSON == next.RetryRunInput {
			next.RetryRunLen++
			switch {
			case next.RetryRunLen == 3:
				out.RetryCount += 2
			case next.RetryRunLen > 3:
				out.RetryCount++
			}
		} else {
			next.RetryRunName = c.ToolName
			next.RetryRunInput = c.InputJSON
			next.RetryRunLen = 1
		}
	}

	// Edit churn: only appends add edit calls; a file counts once.
	out.EditChurnCount = row.EditChurnCount
	editLast := make(map[string]EditChurnState, len(s.EditLast))
	for path, st := range s.EditLast {
		editLast[path] = st
	}
	for _, c := range appended {
		if c.Category != "Edit" && c.Category != "Write" {
			continue
		}
		path := extractFilePath(c.InputJSON)
		if path == "" {
			continue
		}
		st := editLast[path]
		if !st.Counted && st.Last1 > 0 && st.Last2 > 0 &&
			c.MessageOrdinal-st.Last1 < 10 {
			out.EditChurnCount++
			st.Counted = true
		}
		st.Last1, st.Last2 = st.Last2, c.MessageOrdinal
		editLast[path] = st
	}
	next.EditLast = editLast

	// Mid-task compactions: consume appended calls into open boundaries.
	var kept []PendingBoundary
	for _, b := range next.PendingBoundaries {
		after := append([]string(nil), b.AfterNames...)
		decided, counted := false, false
		for _, c := range appended {
			if c.MessageOrdinal <= b.Ordinal {
				continue
			}
			after = append(after, c.ToolName)
			if overlapCount(b.BeforeNames, after) >=
				midTaskOverlapThreshold {
				counted, decided = true, true
				break
			}
			if len(after) >= midTaskWindowAfter {
				decided = true
				break
			}
		}
		if counted {
			out.MidTaskCompactions++
		}
		if !decided {
			b.AfterNames = after
			kept = append(kept, b)
		}
	}
	next.PendingBoundaries = kept

	// Runaway loop: window detector over the full tail with exit folding.
	runaway := s.RunawayHistorical
	windowStart := max(0, s.TotalCalls-TrailingFactCount)
	for i := windowStart; i+12 <= next.TotalCalls; i++ {
		if windowAt(fullTail, windowStart, i) {
			runaway = true
		}
	}
	next.RunawayHistorical = runaway

	// Exact failing runs: unified fold over deep crossing state + full
	// tail facts, latching runs that left the window.
	deep := deepRun{
		sig:      s.ExactRunSig,
		len:      s.ExactRunLen,
		failures: s.ExactRunFailures,
	}
	if deep.sig != "" {
		// The deep run's length includes the old window's leading prefix;
		// strip it so the concatenated fold sees each fact once.
		for _, f := range s.Trailing {
			if f.ExactSignature != deep.sig {
				break
			}
			if f.Failure {
				deep.failures--
			}
			deep.len--
		}
		if deep.len < 0 {
			deep.len = 0
		}
	}
	historical, crossingState, exactQualifies := foldExactRuns(
		deep, fullTail, newCut, s.ExactHistorical,
	)
	next.ExactHistorical = historical
	next.ExactRunSig = crossingState.sig
	next.ExactRunLen = crossingState.len
	next.ExactRunFailures = crossingState.failures

	out.RunawayToolLoopCount = 0
	if exactQualifies || runaway {
		out.RunawayToolLoopCount = 1
	}

	return next, out, true
}

// deepRun is a same-signature run ending at the facts window's left edge,
// summarized as (sig, len, failures).
type deepRun struct {
	sig      string
	len      int
	failures int
}

// foldExactRuns evaluates the exact-failing-run detector over a sequence
// made of an optional deep run followed by post-delta facts. newCut is the
// index into facts where the trailing window starts. It returns the new
// historical latch (qualifying runs that end before the window), the run
// crossing into the window (zero when none), and whether any run —
// historical, crossing, or window-internal — qualifies.
func foldExactRuns(
	deep deepRun, facts []ToolFact, newCut int, historical bool,
) (latch bool, crossing deepRun, qualifies bool) {
	type run struct {
		start    int // index into facts; -1 = deep
		end      int // index into facts; -1 = deep-only
		sig      string
		len      int
		failures int
	}
	var runs []run
	cur := run{
		start: -1, end: -1,
		sig: deep.sig, len: deep.len, failures: deep.failures,
	}
	hasCur := deep.len > 0
	for i, f := range facts {
		if hasCur && f.ExactSignature == cur.sig {
			if cur.start < 0 {
				cur.start = i
			}
			cur.end = i
			cur.len++
			if f.Failure {
				cur.failures++
			}
			continue
		}
		if hasCur {
			runs = append(runs, cur)
		}
		cur = run{
			start: i, end: i,
			sig: f.ExactSignature, len: 1,
		}
		if f.Failure {
			cur.failures = 1
		}
		hasCur = true
	}
	if hasCur {
		runs = append(runs, cur)
	}

	latch = historical
	for _, r := range runs {
		qual := r.len >= 5 && r.failures >= 3
		switch {
		case r.start < 0 && r.end < newCut:
			// Deep run that ended before the window.
			if qual {
				latch = true
			}
		case r.start < 0:
			// Deep run reaching into the window.
			if qual {
				qualifies = true
			}
			crossing = deepRun{sig: r.sig, len: r.len, failures: r.failures}
		case r.end < newCut:
			// Facts run fully before the window.
			if qual {
				latch = true
			}
		case r.start < newCut:
			// Facts run crossing into the window.
			if qual {
				qualifies = true
			}
			crossing = deepRun{sig: r.sig, len: r.len, failures: r.failures}
		default:
			// Window-internal run.
			if qual {
				qualifies = true
			}
		}
	}
	return latch, crossing, qualifies
}

// foldFailureRuns computes the new prefix latch, boundary run, and
// crossing run after the trailing window becomes newBits. seqBefore holds
// the failure bits of every call before the new window: the deep-history
// tail run (crossing from before the old window, as TailFailureRun leading
// trues) followed by the old window facts that slid out. prefixMax is the
// previous longest run fully before the old window. It returns the new
// prefix max, the run ending at the last call before the new window, and
// the full length of the run crossing into the window (0 when the window
// starts with a non-failure).
func foldFailureRuns(
	prefixMax int, seqBefore, newBits []bool,
) (newPrefixMax, tailRun, crossingRun int) {
	newPrefixMax = prefixMax
	i := 0
	for i < len(seqBefore) {
		if !seqBefore[i] {
			i++
			continue
		}
		j := i
		for j+1 < len(seqBefore) && seqBefore[j+1] {
			j++
		}
		if j == len(seqBefore)-1 {
			// The run touches the window boundary. Its length j-i+1 is
			// the full run: seqBefore's head carries the deep portion as
			// leading trues, so no extra accounting is needed.
			tailRun = j - i + 1
		} else {
			// Fully before the window: prefix candidate.
			newPrefixMax = max(newPrefixMax, j-i+1)
		}
		i = j + 1
	}
	if len(newBits) > 0 && newBits[0] {
		head := 0
		for head < len(newBits) && newBits[head] {
			head++
		}
		crossingRun = tailRun + head
	}
	return newPrefixMax, tailRun, crossingRun
}

// maxRunWithin returns the longest true-run fully inside bits.
func maxRunWithin(bits []bool) int {
	best := 0
	i := 0
	for i < len(bits) {
		if !bits[i] {
			i++
			continue
		}
		j := i
		for j+1 < len(bits) && bits[j+1] {
			j++
		}
		best = max(best, j-i+1)
		i = j + 1
	}
	return best
}

func trailingRun(bits []bool) int {
	n := 0
	for i := len(bits) - 1; i >= 0 && bits[i]; i-- {
		n++
	}
	return n
}

// windowAt reports whether the 12-window starting at absolute call index
// start qualifies. facts holds the call facts starting at absolute index
// windowStart.
func windowAt(facts []ToolFact, windowStart, start int) bool {
	first := start - windowStart
	if first < 0 || first+12 > len(facts) {
		return false
	}
	return windowFactsQualify(facts[first : first+12])
}

// windowFactsQualify mirrors hasRunawayToolWindow's single-window test.
func windowFactsQualify(facts []ToolFact) bool {
	if len(facts) < 12 {
		return false
	}
	failures := 0
	classCounts := make(map[string]int, len(facts))
	for _, f := range facts {
		if f.Failure {
			failures++
		}
		classCounts[f.CommandClass]++
	}
	if failures >= 6 {
		return true
	}
	return failures >= 3 && dominantCount(classCounts) >= 10
}

// mergeFacts merges two position-ordered fact windows, preferring newer
// facts when positions overlap, sorted by position.
func mergeFacts(old, newer []ToolFact) []ToolFact {
	byPos := make(map[CallPos]ToolFact, len(old)+len(newer))
	seen := make(map[CallPos]bool, len(old)+len(newer))
	var order []CallPos
	for _, f := range old {
		if !seen[f.CallPos] {
			seen[f.CallPos] = true
			order = append(order, f.CallPos)
		}
		byPos[f.CallPos] = f
	}
	for _, f := range newer {
		if !seen[f.CallPos] {
			seen[f.CallPos] = true
			order = append(order, f.CallPos)
		}
		byPos[f.CallPos] = f
	}
	slices.SortFunc(order, func(a, b CallPos) int {
		if a.MessageOrdinal != b.MessageOrdinal {
			return a.MessageOrdinal - b.MessageOrdinal
		}
		return a.CallIndex - b.CallIndex
	})
	merged := make([]ToolFact, 0, len(order))
	for _, pos := range order {
		merged = append(merged, byPos[pos])
	}
	return merged
}

// MarshalBinary implements encoding.BinaryMarshaler for the state row.
func (s *IncrementalState) MarshalBinary() ([]byte, error) {
	return json.Marshal(s)
}

// UnmarshalBinary implements encoding.BinaryUnmarshaler. A malformed or
// version-mismatched payload is rejected so the caller falls back.
func (s *IncrementalState) UnmarshalBinary(data []byte) error {
	if err := json.Unmarshal(data, s); err != nil {
		return fmt.Errorf("decoding incremental signal state: %w", err)
	}
	if s.CodecVersion != IncrementalStateCodecVersion {
		return fmt.Errorf(
			"incremental signal state codec %d != %d",
			s.CodecVersion, IncrementalStateCodecVersion,
		)
	}
	return nil
}
