package parser

import (
	"slices"
	"sort"
	"strconv"
)

// codebuddyCBCReplay returns the indexes of the producer's active history,
// following the CodeBuddy CLI's resolveReplayHistory with custom items
// excluded from the returned path, plus the standalone model-usage items
// whose parent is on it.
func codebuddyCBCReplay(items []codebuddyCBCItem) []int {
	return codebuddyWithUsageItems(items, codebuddyReplayHistory(items))
}

func codebuddyReplayHistory(items []codebuddyCBCItem) []int {
	hasCompacted := false
	hasParent := false
	hasFilter := false
	hasResend := false
	for _, it := range items {
		if it.logicalParentID != "" && it.parentID == "" {
			hasCompacted = true
		}
		if it.parentID != "" {
			hasParent = true
		}
		if it.providerData().Get("contentFilterNotice").Bool() {
			hasFilter = true
		}
		if it.itemType == "resend-fork-notice" {
			hasResend = true
		}
	}
	// A trailing resend notice means the user rewound and has not sent the
	// replacement prompt yet; the history ends at the notice's parent.
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it.itemType == "resend-fork-notice" {
			if it.parentID == "" {
				return nil
			}
			return codebuddyActiveHistory(items, it.parentID, hasCompacted)
		}
		if !it.custom() {
			break
		}
	}
	if !hasParent || (!hasFilter && !hasResend) {
		out := make([]int, 0, len(items))
		for i, it := range items {
			if !it.custom() {
				out = append(out, i)
			}
		}
		return out
	}
	return codebuddyActiveHistory(
		items, codebuddyColdStartTarget(items), hasCompacted,
	)
}

// codebuddyWithUsageItems adds the model-usage items whose parent is on path
// or that have no parent. The producer leaves them out of the replayed
// history, but each records a billed response, so it belongs to the path its
// parent is on.
func codebuddyWithUsageItems(items []codebuddyCBCItem, path []int) []int {
	onPath := make(map[string]bool, len(path))
	for _, idx := range path {
		if items[idx].id != "" {
			onPath[items[idx].id] = true
		}
	}
	out := slices.Clone(path)
	for i, it := range items {
		if it.itemType == "model-usage" && (it.parentID == "" || onPath[it.parentID]) {
			out = append(out, i)
		}
	}
	if len(out) == len(path) {
		return path
	}
	sort.Ints(out)
	return out
}

func codebuddyColdStartTarget(items []codebuddyCBCItem) string {
	notice := -1
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].itemType == "resend-fork-notice" {
			notice = i
			break
		}
	}
	if notice >= 0 {
		for i := len(items) - 1; i > notice; i-- {
			if items[i].id != "" && !items[i].custom() {
				return items[i].id
			}
		}
		return items[notice].parentID
	}
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].id != "" && !items[i].custom() {
			return items[i].id
		}
	}
	return ""
}

// codebuddyActiveHistory walks parent links backward from target exactly as
// the CLI's getActiveHistory does, including adjacent items that repeat an
// id and the logical-parent fallback for compacted history.
func codebuddyActiveHistory(
	items []codebuddyCBCItem, target string, hasCompacted bool,
) []int {
	if target == "" {
		return nil
	}
	var picked []int
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].id != target {
			continue
		}
		picked = append(picked, i)
		for i-1 >= 0 && items[i-1].id == target {
			i--
			picked = append(picked, i)
		}
		it := items[i]
		compact := it.providerData().Get("agent").Str == "compact"
		next := it.parentID
		if next == "" && (hasCompacted || i == len(items)-1 || compact) {
			next = it.logicalParentID
		}
		target = next
		if target == "" {
			break
		}
	}
	slices.Reverse(picked)
	out := picked[:0]
	for _, idx := range picked {
		if !items[idx].custom() {
			out = append(out, idx)
		}
	}
	return out
}

// codebuddyCBCBranch is one preserved alternative path. key names the fork
// session under the conversation's session.
type codebuddyCBCBranch struct {
	indexes []int
	key     string
}

// codebuddyCBCPrefixRepeat bounds how many times over the alternative items'
// size shared ancestors may be repeated across branches. A branch past the
// bound keeps only the items it does not share with an earlier branch.
const codebuddyCBCPrefixRepeat = 4

// codebuddyCBCNode is one alternative turn item, or a run of adjacent items
// that repeat one id, in the tree of items outside the active path.
type codebuddyCBCNode struct {
	items    []int
	usage    []int
	children []int
	parent   int
	// pathBytes is the raw size of this node and its ancestors.
	pathBytes int
}

// codebuddyCBCAlternatives splits the conversation items outside the active
// path into replayable branches, one for each leaf of the tree their parent
// links form. A branch repeats the alternative ancestors it shares with
// another branch, so each reads as one coherent history; repeated responses
// keep their usage identity and count once. The first branch under each
// root is keyed by the root's id, as a branch without divergent
// continuations always was; each later branch adds the id of the item where
// it diverges.
func codebuddyCBCAlternatives(
	items []codebuddyCBCItem, active []int,
) []codebuddyCBCBranch {
	onPath := make(map[int]bool, len(active))
	for _, idx := range active {
		onPath[idx] = true
	}
	byID := make(map[string]int, len(items))
	for i, it := range items {
		if it.id == "" {
			continue
		}
		if _, exists := byID[it.id]; !exists {
			byID[it.id] = i
		}
	}
	var nodes []codebuddyCBCNode
	nodeOf := make(map[int]int)
	var usage []int
	for i, it := range items {
		if onPath[i] {
			continue
		}
		if it.itemType == "model-usage" {
			usage = append(usage, i)
			continue
		}
		if it.custom() {
			continue
		}
		if it.providerData().Get("contentFilterNotice").Bool() {
			// The notice replaced a filtered turn; it is not a user branch.
			continue
		}
		if n, ok := nodeOf[i-1]; ok && it.id != "" && items[i-1].id == it.id {
			nodes[n].items = append(nodes[n].items, i)
			nodeOf[i] = n
			continue
		}
		nodes = append(nodes, codebuddyCBCNode{items: []int{i}, parent: -1})
		nodeOf[i] = len(nodes) - 1
	}
	if len(nodes) == 0 {
		return nil
	}
	// parentNode finds the alternative node an item continues. Parents
	// precede their children, so the links form a forest even when a
	// transcript's ids repeat or loop.
	parentNode := func(idx int) int {
		it := items[idx]
		parent := it.parentID
		if parent == "" {
			parent = it.logicalParentID
		}
		p, ok := byID[parent]
		if !ok || p >= idx {
			return -1
		}
		if n, ok := nodeOf[p]; ok {
			return n
		}
		return -1
	}
	total := 0
	for n := range nodes {
		node := &nodes[n]
		node.parent = parentNode(node.items[0])
		if node.parent >= 0 {
			nodes[node.parent].children = append(nodes[node.parent].children, n)
		}
	}
	for _, idx := range usage {
		if n := parentNode(idx); n >= 0 {
			nodes[n].usage = append(nodes[n].usage, idx)
		}
	}
	for n := range nodes {
		size := 0
		for _, idx := range slices.Concat(nodes[n].items, nodes[n].usage) {
			size += len(items[idx].raw.Raw)
		}
		total += size
		if p := nodes[n].parent; p >= 0 {
			size += nodes[p].pathBytes
		}
		nodes[n].pathBytes = size
	}
	budget := codebuddyCBCPrefixRepeat * total
	used := make(map[string]bool)
	emitted := make([]bool, len(nodes))
	var branches []codebuddyCBCBranch
	for root := range nodes {
		if nodes[root].parent >= 0 {
			continue
		}
		rootKey := items[nodes[root].items[0]].id
		stack := []int{root}
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if children := nodes[n].children; len(children) > 0 {
				for _, c := range slices.Backward(children) {
					stack = append(stack, c)
				}
				continue
			}
			// The leaf's own items run up to the first ancestor an earlier
			// branch already holds.
			var path []int
			top := n
			for m := n; m >= 0 && !emitted[m]; m = nodes[m].parent {
				emitted[m] = true
				path = append(path, m)
				top = m
			}
			key := rootKey
			if top != root {
				key += "-" + items[nodes[top].items[0]].id
				if p := nodes[top].parent; nodes[p].pathBytes <= budget {
					budget -= nodes[p].pathBytes
					for m := p; m >= 0; m = nodes[m].parent {
						path = append(path, m)
					}
				}
			}
			if used[key] {
				// Ids missing or repeated in the transcript still give each
				// branch its own session.
				key += "-" + strconv.Itoa(items[nodes[n].items[0]].index)
			}
			used[key] = true
			var indexes []int
			for _, m := range path {
				indexes = append(indexes, nodes[m].items...)
				indexes = append(indexes, nodes[m].usage...)
			}
			sort.Ints(indexes)
			branches = append(branches, codebuddyCBCBranch{indexes: indexes, key: key})
		}
	}
	return branches
}
