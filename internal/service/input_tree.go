package service

import (
	"cmp"
	"context"
	"slices"

	"go.kenn.io/agentsview/internal/db"
)

// Limits that keep the input tree of a large fork family bounded. A tree that
// hits either one is returned with Truncated set.
const (
	inputTreeMaxSessions = 200
	inputTreeMaxNodes    = 5000
)

// InputTree is the branch structure of the user inputs in a session's fork
// family: the sessions linked by forks and continuations. Each node is one
// input. A session's inputs, including the prefix it inherits as a fork or
// continuation, form one path that starts at a top-level node. Inputs that
// sessions share appear once, so the tree branches where a fork's inputs
// diverge from the session it was forked from, and a continuation's inputs
// extend the path of the session it continues.
type InputTree struct {
	ActiveSessionID string `json:"active_session_id"`
	// RootSessionID is the session the fork family starts from.
	RootSessionID string `json:"root_session_id"`
	// Nodes lists the tree depth first: each parent comes before its
	// children, and siblings follow the order their sessions started.
	Nodes []InputTreeNode `json:"nodes"`
	// Sessions are the members of the fork family, root first and the rest
	// in the order they started.
	Sessions  []db.Session `json:"sessions"`
	Truncated bool         `json:"truncated"`
}

// InputTreeNode is one input in an InputTree.
type InputTreeNode struct {
	// Parent is the index of the parent node in Nodes, or -1 for an input
	// that starts a path.
	Parent int `json:"parent"`
	// SessionID and Ordinal identify the message that holds the input.
	SessionID string `json:"session_id"`
	Ordinal   int    `json:"ordinal"`
	Timestamp string `json:"timestamp,omitempty"`
	Preview   string `json:"preview"`
	IsShell   bool   `json:"is_shell"`
	// OnActivePath marks the inputs on the active session's path.
	OnActivePath bool `json:"on_active_path"`
	// ActiveOrdinal is the input's ordinal in the active session's message
	// view, which includes the context a fork inherits. It is set only for
	// the inputs on the active session's path that its message view shows,
	// so it is unset for the inputs a continuation inherits.
	ActiveOrdinal *int `json:"active_ordinal,omitempty"`
	// EndSessionIDs lists the sessions whose path ends at this input. Every
	// leaf ends at least one session.
	EndSessionIDs []string `json:"end_session_ids"`
}

// InputTreeReader is implemented by services that can build input trees
// from the archive they read.
type InputTreeReader interface {
	// InputTree returns the input tree of the fork family that contains
	// the session, or nil when the session does not exist.
	InputTree(ctx context.Context, id string) (*InputTree, error)
}

// inputTreeStore is the subset of db.Store the input tree reads.
type inputTreeStore interface {
	forkMessageSource
	GetChildSessions(ctx context.Context, parentID string) ([]db.Session, error)
}

func (b *directBackend) InputTree(
	ctx context.Context, id string,
) (*InputTree, error) {
	src := newCachedInputTreeStore(b.db)
	active, err := src.GetSession(ctx, id)
	if err != nil || active == nil {
		return nil, err
	}

	family, truncated, err := forkFamily(ctx, src, active)
	if err != nil {
		return nil, err
	}

	tree := newInputTreeBuilder()
	for _, session := range family {
		_, own, inherited, err := b.forkContext(ctx, src, session.ID)
		if err != nil {
			return nil, err
		}
		continued, err := continuedContext(ctx, src, session, own, inherited)
		if err != nil {
			return nil, err
		}
		steps := inputPathSteps(continued, inherited, own, session.ID == id)
		if !tree.insert(session.ID, steps) {
			truncated = true
			break
		}
	}

	sessions := make([]db.Session, 0, len(family))
	for _, session := range family {
		sessions = append(sessions, *session)
	}
	return &InputTree{
		ActiveSessionID: id,
		RootSessionID:   family[0].ID,
		Nodes:           tree.nodesDepthFirst(),
		Sessions:        sessions,
		Truncated:       truncated,
	}, nil
}

// forkFamily returns the sessions linked to active by fork and continuation
// relationships: the nearest ancestor that is neither, and every fork and
// continuation below it. The root comes first, then the rest in the order
// they started. The active session and its ancestors are kept when the
// family is larger than the limit.
func forkFamily(
	ctx context.Context, src inputTreeStore, active *db.Session,
) ([]*db.Session, bool, error) {
	chain := []*db.Session{active}
	seen := map[string]bool{active.ID: true}
	truncated := false
	for current := active; isInputTreeLink(current); {
		parentID := *current.ParentSessionID
		if seen[parentID] {
			truncated = true
			break
		}
		parent, err := src.GetSession(ctx, parentID)
		if err != nil {
			return nil, false, err
		}
		if parent == nil {
			break
		}
		seen[parentID] = true
		chain = append(chain, parent)
		current = parent
	}
	slices.Reverse(chain)

	family := slices.Clone(chain)
	queue := slices.Clone(chain)
collect:
	for len(queue) > 0 {
		children, err := src.GetChildSessions(ctx, queue[0].ID)
		if err != nil {
			return nil, false, err
		}
		queue = queue[1:]
		for i := range children {
			child := &children[i]
			if !isInputTreeRelationship(child.RelationshipType) ||
				seen[child.ID] {
				continue
			}
			if len(family) >= inputTreeMaxSessions {
				truncated = true
				break collect
			}
			seen[child.ID] = true
			family = append(family, child)
			queue = append(queue, child)
		}
	}

	slices.SortStableFunc(family[1:], func(a, b *db.Session) int {
		return cmp.Or(
			compareStartedAt(a.StartedAt, b.StartedAt),
			cmp.Compare(a.ID, b.ID),
		)
	})
	return family, truncated, nil
}

// isInputTreeLink reports whether a session's inputs follow on from its
// parent's: a fork branches from them and a continuation carries them on.
func isInputTreeLink(s *db.Session) bool {
	return isInputTreeRelationship(s.RelationshipType) &&
		s.ParentSessionID != nil &&
		*s.ParentSessionID != ""
}

func isInputTreeRelationship(relationship string) bool {
	return relationship == "fork" || relationship == "continuation"
}

// continuedContext returns the messages a session's path inherits beyond
// the fork context its message view shows. That context follows forks up
// to the first ancestor that is not one. When that ancestor, or the session
// itself, is a continuation, the path goes on with the session it continues,
// up to where the continuation took over, and so on through any forks and
// continuations above it.
func continuedContext(
	ctx context.Context,
	src forkMessageSource,
	session *db.Session,
	own, inherited []db.Message,
) ([]db.Message, error) {
	top := session
	visited := map[string]bool{session.ID: true}
	for isForkWithParent(top) {
		parent, err := src.GetSession(ctx, *top.ParentSessionID)
		if err != nil || parent == nil || visited[parent.ID] {
			return nil, err
		}
		visited[parent.ID] = true
		top = parent
	}
	if !isInputTreeLink(top) {
		return nil, nil
	}

	// The boundary is where top's messages on this path start: its own
	// when it is the session, otherwise the part the fork context took.
	topMsgs := own
	if top.ID != session.ID {
		topMsgs = nil
		for _, msg := range inherited {
			if msg.SessionID == top.ID {
				topMsgs = append(topMsgs, msg)
			}
		}
	}
	boundary, hasBoundary := forkBoundaryTime(topMsgs, top.StartedAt)
	return ancestorMessages(
		ctx, src, *top.ParentSessionID, boundary, hasBoundary, visited,
		isInputTreeLink,
	)
}

// compareStartedAt orders sessions by start time and puts sessions without
// one last.
func compareStartedAt(a, b *string) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return cmp.Compare(*a, *b)
}

// inputPathStep is one input on a session's path through the tree.
type inputPathStep struct {
	sessionID     string
	item          InputOutlineItem
	active        bool
	activeOrdinal *int
}

// inputPathSteps lists the inputs on a session's path: those it inherits
// beyond its fork context, those in the fork context, then its own. On the
// active session's path, each step the message view shows records the
// ordinal it has there.
func inputPathSteps(
	continued, inherited, own []db.Message, active bool,
) []inputPathStep {
	steps := make([]inputPathStep, 0)
	add := func(msg db.Message, viewOrdinal *int) {
		if !isOutlineInput(msg) {
			return
		}
		item, ok := inputOutlineItem(msg.Ordinal, msg.Timestamp, msg.Content)
		if !ok {
			return
		}
		step := inputPathStep{
			sessionID: msg.SessionID,
			item:      item,
			active:    active,
		}
		if active {
			step.activeOrdinal = viewOrdinal
		}
		steps = append(steps, step)
	}
	for _, msg := range continued {
		add(msg, nil)
	}
	for i, msg := range inherited {
		viewOrdinal := forkContextOrdinal(i, len(inherited))
		add(msg, &viewOrdinal)
	}
	for _, msg := range own {
		viewOrdinal := msg.Ordinal
		add(msg, &viewOrdinal)
	}
	return steps
}

// inputTreeEdge identifies a node by its parent and the message it holds.
type inputTreeEdge struct {
	parent    int
	sessionID string
	ordinal   int
}

type inputTreeBuildNode struct {
	node     InputTreeNode
	children []int
}

// inputTreeBuilder merges session paths into a tree. Paths share a node for
// as long as they hold the same messages in the same order.
type inputTreeBuilder struct {
	nodes    []inputTreeBuildNode
	topLevel []int
	edges    map[inputTreeEdge]int
}

func newInputTreeBuilder() *inputTreeBuilder {
	return &inputTreeBuilder{edges: map[inputTreeEdge]int{}}
}

// insert adds a session's path and marks its last input as the session's
// end. It reports false when the node limit stopped the path short.
func (t *inputTreeBuilder) insert(
	sessionID string, steps []inputPathStep,
) bool {
	parent := -1
	for _, step := range steps {
		edge := inputTreeEdge{
			parent:    parent,
			sessionID: step.sessionID,
			ordinal:   step.item.Ordinal,
		}
		idx, ok := t.edges[edge]
		if !ok {
			if len(t.nodes) >= inputTreeMaxNodes {
				return false
			}
			idx = len(t.nodes)
			t.nodes = append(t.nodes, inputTreeBuildNode{
				node: InputTreeNode{
					Parent:        parent,
					SessionID:     step.sessionID,
					Ordinal:       step.item.Ordinal,
					Timestamp:     step.item.Timestamp,
					Preview:       step.item.Preview,
					IsShell:       step.item.IsShell,
					EndSessionIDs: []string{},
				},
			})
			t.edges[edge] = idx
			if parent < 0 {
				t.topLevel = append(t.topLevel, idx)
			} else {
				t.nodes[parent].children = append(
					t.nodes[parent].children, idx,
				)
			}
		}
		if step.active {
			t.nodes[idx].node.OnActivePath = true
		}
		if step.activeOrdinal != nil {
			t.nodes[idx].node.ActiveOrdinal = step.activeOrdinal
		}
		parent = idx
	}
	if parent >= 0 {
		t.nodes[parent].node.EndSessionIDs = append(
			t.nodes[parent].node.EndSessionIDs, sessionID,
		)
	}
	return true
}

// nodesDepthFirst returns the nodes in depth-first order, with Parent
// pointing into the returned slice.
func (t *inputTreeBuilder) nodesDepthFirst() []InputTreeNode {
	out := make([]InputTreeNode, 0, len(t.nodes))
	position := make([]int, len(t.nodes))
	stack := slices.Clone(t.topLevel)
	slices.Reverse(stack)
	for len(stack) > 0 {
		idx := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		node := t.nodes[idx].node
		if node.Parent >= 0 {
			node.Parent = position[node.Parent]
		}
		position[idx] = len(out)
		out = append(out, node)
		for _, child := range slices.Backward(t.nodes[idx].children) {
			stack = append(stack, child)
		}
	}
	return out
}

// cachedInputTreeStore memoizes reads, since every fork in a family reads
// the messages of the sessions above it.
type cachedInputTreeStore struct {
	store    inputTreeStore
	sessions map[string]*db.Session
	messages map[string][]db.Message
}

func newCachedInputTreeStore(store inputTreeStore) *cachedInputTreeStore {
	return &cachedInputTreeStore{
		store:    store,
		sessions: map[string]*db.Session{},
		messages: map[string][]db.Message{},
	}
}

func (c *cachedInputTreeStore) GetSession(
	ctx context.Context, id string,
) (*db.Session, error) {
	if session, ok := c.sessions[id]; ok {
		return session, nil
	}
	session, err := c.store.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	c.sessions[id] = session
	return session, nil
}

func (c *cachedInputTreeStore) GetAllMessages(
	ctx context.Context, sessionID string,
) ([]db.Message, error) {
	if msgs, ok := c.messages[sessionID]; ok {
		return msgs, nil
	}
	msgs, err := c.store.GetAllMessages(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	c.messages[sessionID] = msgs
	return msgs, nil
}

func (c *cachedInputTreeStore) GetChildSessions(
	ctx context.Context, parentID string,
) ([]db.Session, error) {
	return c.store.GetChildSessions(ctx, parentID)
}
