// ABOUTME: Tests for DAG fork detection in Claude JSONL session files.
// ABOUTME: Validates linear, large-gap fork, small-gap retry, and backward compat scenarios.
package parser

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func parseTestContent(t *testing.T, name, content string, expectedLen int) []ParseResult {
	t.Helper()
	path := createTestFile(t, name, content)
	results, err := parseClaudeSession(path, "proj", "local")
	require.NoError(t, err, "ParseClaudeSession")
	require.Len(t, results, expectedLen)
	return results
}

func formatTime(ts time.Time) string {
	return ts.Format(time.RFC3339)
}

func TestForkDetection_LinearSession(t *testing.T) {
	// Linear chain: a -> b -> c -> d, all with uuid/parentUuid.
	// Should return 1 result with 4 messages.
	content := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID("2024-01-01T10:00:00Z", "hello", "a", "").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:01Z", "hi there", "b", "a").
		AddClaudeUserWithUUID("2024-01-01T10:00:02Z", "next question", "c", "b").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:03Z", "answer", "d", "c").
		String()

	results := parseTestContent(t, "linear.jsonl", content, 1)

	assertSessionMeta(t, &results[0].Session, "linear", "proj", AgentClaude)
	assertMessageCount(t, len(results[0].Messages), 4)
}

func TestForkDetection_LargeGapFork(t *testing.T) {
	// Main branch: a->b->c->d->e->f->g->h (4+ user turns after fork)
	// Fork from b: i->j
	// Fork point is at node b, which has children c (first) and i.
	// First branch (c side) has user turns at c, e, g = 3 user turns,
	// but we need >3, so add more.
	//
	// Let's make:
	//   a(user) -> b(asst) -> c(user) -> d(asst) -> e(user) -> f(asst)
	//                                    -> g(user) -> h(asst)
	//                      -> i(user) -> j(asst)   [fork from b]
	//
	// User turns on first branch after fork point b: c, e, g = 3,
	// need >3 so add one more pair.
	//   a(user) -> b(asst) -> c(user) -> d(asst) -> e(user) -> f(asst)
	//                                    -> g(user) -> h(asst) -> k(user) -> l(asst)
	//                      -> i(user) -> j(asst)
	//
	// User turns on first branch from c onward: c, e, g, k = 4 > 3 = large gap.
	content := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID("2024-01-01T10:00:00Z", "hello", "a", "").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:01Z", "hi", "b", "a").
		AddClaudeUserWithUUID("2024-01-01T10:00:02Z", "q1", "c", "b").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:03Z", "a1", "d", "c").
		AddClaudeUserWithUUID("2024-01-01T10:00:04Z", "q2", "e", "d").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:05Z", "a2", "f", "e").
		AddClaudeUserWithUUID("2024-01-01T10:00:06Z", "q3", "g", "f").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:07Z", "a3", "h", "g").
		AddClaudeUserWithUUID("2024-01-01T10:00:08Z", "q4", "k", "h").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:09Z", "a4", "l", "k").
		// Fork branch from b
		AddClaudeUserWithUUID("2024-01-01T10:01:00Z", "fork q1", "i", "b").
		AddClaudeAssistantWithUUID("2024-01-01T10:01:01Z", "fork a1", "j", "i").
		String()

	results := parseTestContent(t, "fork.jsonl", content, 2)

	// Main session: all entries on first branch (a,b,c,d,e,f,g,h,k,l)
	main := results[0]
	assertSessionMeta(t, &main.Session, "fork", "proj", AgentClaude)
	assertMessageCount(t, len(main.Messages), 10)
	assert.Empty(t, main.Session.ParentSessionID, "main ParentSessionID")

	// Fork session: entries on fork branch (i,j)
	fork := results[1]
	assert.Equal(t, "fork-i", fork.Session.ID, "fork session ID")
	assertMessageCount(t, len(fork.Messages), 2)
	assert.Equal(t, "fork", fork.Session.ParentSessionID, "fork ParentSessionID")
	assert.Equal(t, RelFork, fork.Session.RelationshipType, "fork RelationshipType")
	assert.Equal(t, "fork q1", fork.Session.FirstMessage, "fork FirstMessage")
}

func TestForkDetection_SmallGapRetry(t *testing.T) {
	// Main: a(user)->b(asst)->c(user)->d(asst) (1 user turn after fork = small gap)
	// Retry from b: e(user)->f(asst)
	// First branch from b has c,d — only 1 user turn (c). ≤3 = small gap.
	// Should follow LAST child (e), so result is: a,b,e,f
	content := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID("2024-01-01T10:00:00Z", "hello", "a", "").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:01Z", "hi", "b", "a").
		AddClaudeUserWithUUID("2024-01-01T10:00:02Z", "first try", "c", "b").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:03Z", "first answer", "d", "c").
		// Retry from b (later in file)
		AddClaudeUserWithUUID("2024-01-01T10:01:00Z", "retry", "e", "b").
		AddClaudeAssistantWithUUID("2024-01-01T10:01:01Z", "retry answer", "f", "e").
		String()

	results := parseTestContent(t, "retry.jsonl", content, 1)

	// Latest branch wins: a, b, e, f
	assertMessageCount(t, len(results[0].Messages), 4)
	assertMessage(t, results[0].Messages[0], RoleUser, "hello")
	assertMessage(t, results[0].Messages[1], RoleAssistant, "hi")
	assertMessage(t, results[0].Messages[2], RoleUser, "retry")
	assertMessage(t, results[0].Messages[3], RoleAssistant, "retry answer")
}

func TestForkDetection_NoUUIDs(t *testing.T) {
	// Entries without uuid fields — should work as before, 1 result.
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T10:00:00Z", "hello").
		AddClaudeAssistant("2024-01-01T10:00:01Z", "hi").
		AddClaudeUser("2024-01-01T10:00:02Z", "bye").
		AddClaudeAssistant("2024-01-01T10:00:03Z", "goodbye").
		String()

	results := parseTestContent(t, "nouuid.jsonl", content, 1)

	assertMessageCount(t, len(results[0].Messages), 4)
	assertMessage(t, results[0].Messages[0], RoleUser, "hello")
}

func TestForkDetection_MixedUUIDs(t *testing.T) {
	// Some entries have uuid, some don't — fall back to linear.
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T10:00:00Z", "no uuid").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:01Z", "has uuid", "x", "").
		AddClaudeUser("2024-01-01T10:00:02Z", "no uuid again").
		String()

	results := parseTestContent(t, "mixed.jsonl", content, 1)

	assertMessageCount(t, len(results[0].Messages), 3)
}

func TestForkDetection_NestedFork(t *testing.T) {
	// Main: a->b->c->d->e->f->g->h->k->l (5 user turns)
	// Fork from b: m->n->o->p->q->r->s->t->u->v (5 user turns on fork branch)
	//   Nested fork from n: w->x (fork within the fork branch)
	// Fork from b has 5 user turns on first child path (m,o,q,s,u) > 3 = large gap
	// Nested fork from n has 4 user turns on first child path (o,q,s,u) > 3 = large gap
	content := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID("2024-01-01T10:00:00Z", "start", "a", "").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:01Z", "ok", "b", "a").
		// Main branch from b
		AddClaudeUserWithUUID("2024-01-01T10:00:02Z", "main1", "c", "b").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:03Z", "m-ok1", "d", "c").
		AddClaudeUserWithUUID("2024-01-01T10:00:04Z", "main2", "e", "d").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:05Z", "m-ok2", "f", "e").
		AddClaudeUserWithUUID("2024-01-01T10:00:06Z", "main3", "g", "f").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:07Z", "m-ok3", "h", "g").
		AddClaudeUserWithUUID("2024-01-01T10:00:08Z", "main4", "k", "h").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:09Z", "m-ok4", "l", "k").
		// Fork branch from b
		AddClaudeUserWithUUID("2024-01-01T10:01:00Z", "fork1", "m", "b").
		AddClaudeAssistantWithUUID("2024-01-01T10:01:01Z", "f-ok1", "n", "m").
		AddClaudeUserWithUUID("2024-01-01T10:01:02Z", "fork2", "o", "n").
		AddClaudeAssistantWithUUID("2024-01-01T10:01:03Z", "f-ok2", "p", "o").
		AddClaudeUserWithUUID("2024-01-01T10:01:04Z", "fork3", "q", "p").
		AddClaudeAssistantWithUUID("2024-01-01T10:01:05Z", "f-ok3", "r", "q").
		AddClaudeUserWithUUID("2024-01-01T10:01:06Z", "fork4", "s", "r").
		AddClaudeAssistantWithUUID("2024-01-01T10:01:07Z", "f-ok4", "tt", "s").
		AddClaudeUserWithUUID("2024-01-01T10:01:08Z", "fork5", "u", "tt").
		AddClaudeAssistantWithUUID("2024-01-01T10:01:09Z", "f-ok5", "v", "u").
		// Nested fork from n (within the fork branch)
		AddClaudeUserWithUUID("2024-01-01T10:02:00Z", "nested", "w", "n").
		AddClaudeAssistantWithUUID("2024-01-01T10:02:01Z", "n-ok", "x", "w").
		String()

	// Expect 3 results: main, nested fork from n, fork from b
	// (depth-first: nested fork discovered during recursive walk of b's fork)
	results := parseTestContent(t, "nested-fork.jsonl", content, 3)

	// Main: a,b,c,d,e,f,g,h,k,l = 10 messages
	assertMessageCount(t, len(results[0].Messages), 10)

	// Nested fork from n (discovered first during depth-first walk): w,x = 2 messages
	nested := results[1]
	assert.Equal(t, "nested-fork-w", nested.Session.ID, "nested ID")
	assertMessageCount(t, len(nested.Messages), 2)
	assert.Equal(t, RelFork, nested.Session.RelationshipType, "nested RelationshipType")
	// Nested fork's parent should be the fork branch it split
	// from, not the root session.
	assert.Equal(t, "nested-fork-m", nested.Session.ParentSessionID, "nested ParentSessionID")

	// Fork from b: m,n,o,p,q,r,s,tt,u,v = 10 messages
	fork := results[2]
	assert.Equal(t, "nested-fork-m", fork.Session.ID, "fork ID")
	assertMessageCount(t, len(fork.Messages), 10)
	assert.Equal(t, RelFork, fork.Session.RelationshipType, "fork RelationshipType")
	// Fork from b's parent should be the root session.
	assert.Equal(t, "nested-fork", fork.Session.ParentSessionID, "fork ParentSessionID")
}

func TestForkDetection_MultipleRoots(t *testing.T) {
	// Two entries with empty parentUuid = two roots.
	// A well-formed DAG has exactly one root, so multiple roots
	// should fall back to linear parsing, returning all messages.
	content := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID("2024-01-01T10:00:00Z", "root one", "a", "").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:01Z", "reply one", "b", "a").
		AddClaudeUserWithUUID("2024-01-01T10:00:02Z", "root two", "c", "").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:03Z", "reply two", "d", "c").
		String()

	results := parseTestContent(t, "multi-root.jsonl", content, 1)

	// All 4 messages must be present.
	assertMessageCount(t, len(results[0].Messages), 4)
	assertMessage(t, results[0].Messages[0], RoleUser, "root one")
	assertMessage(t, results[0].Messages[1], RoleAssistant, "reply one")
	assertMessage(t, results[0].Messages[2], RoleUser, "root two")
	assertMessage(t, results[0].Messages[3], RoleAssistant, "reply two")
}

func TestForkDetection_DisconnectedParent(t *testing.T) {
	// Entry "c" has parentUuid "nonexistent" which doesn't match
	// any entry's uuid. This means the DAG is disconnected, so
	// we should fall back to linear parsing.
	content := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID("2024-01-01T10:00:00Z", "hello", "a", "").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:01Z", "hi", "b", "a").
		AddClaudeUserWithUUID("2024-01-01T10:00:02Z", "orphan", "c", "nonexistent").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:03Z", "orphan reply", "d", "c").
		String()

	results := parseTestContent(t, "disconnected.jsonl", content, 1)

	// All 4 messages must be present.
	assertMessageCount(t, len(results[0].Messages), 4)
	assertMessage(t, results[0].Messages[0], RoleUser, "hello")
	assertMessage(t, results[0].Messages[1], RoleAssistant, "hi")
	assertMessage(t, results[0].Messages[2], RoleUser, "orphan")
	assertMessage(t, results[0].Messages[3], RoleAssistant, "orphan reply")
}

func TestSessionBoundsIncludeNonMessageEvents(t *testing.T) {
	// A trailing queue-operation event has a later timestamp
	// than any user/assistant message. Session endedAt should
	// still reflect that later timestamp.
	queueLine := `{"type":"queue-operation","operation":"enqueue",` +
		`"timestamp":"2024-01-01T11:00:00Z","content":"{}"}`

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T10:00:00Z", "hello").
		AddClaudeAssistant("2024-01-01T10:00:01Z", "hi").
		AddRaw(queueLine).
		String()

	results := parseTestContent(t, "queue-ts.jsonl", content, 1)

	sess := results[0].Session
	assert.Equal(t, "2024-01-01T11:00:00Z", formatTime(sess.EndedAt), "EndedAt")
}

func TestSessionBoundsStartedAtFromLeadingEvent(t *testing.T) {
	// A leading non-message event has an earlier timestamp
	// than the first user message. StartedAt should reflect it.
	earlyLine := `{"type":"queue-operation","operation":"enqueue",` +
		`"timestamp":"2024-01-01T09:00:00Z","content":"{}"}`

	content := testjsonl.NewSessionBuilder().
		AddRaw(earlyLine).
		AddClaudeUser("2024-01-01T10:00:00Z", "hello").
		AddClaudeAssistant("2024-01-01T10:00:01Z", "hi").
		String()

	results := parseTestContent(t, "early-queue.jsonl", content, 1)

	sess := results[0].Session
	assert.Equal(t, "2024-01-01T09:00:00Z", formatTime(sess.StartedAt), "StartedAt")
}

func TestForkDetection_NestedForkCountsFullSubtree(t *testing.T) {
	// Regression test: when the first child at a fork point
	// itself contains nested forks early in its chain, the
	// old first-child-only countUserTurns would see only 1
	// user turn (following first children that dead-end
	// quickly) and treat the entire large branch as a small
	// retry, discarding it.
	//
	// DAG:  root(a) -> b (fork point)
	//   First child:  c -> d (fork) -> e -> f -> g -> h -> i -> j
	//                          \-> d2 (retry, 1 entry)
	//   Second child: z (1 entry, the "retry")
	//
	// The first child subtree has 5 user turns total (c,e,g,i
	// plus d2). With first-child-only traversal, the path
	// c->d->d2 sees only 1 user turn (c is user, d is asst,
	// d2 is user but d2 is the SECOND child not the first) --
	// actually c->d->(first child of d's fork)=e gives more.
	// Let's build a clearer case: the first child at the fork
	// is a dead-end assistant reply, so first-child traversal
	// stops after 0 user turns.
	//
	// DAG:  root(a) -> b (fork)
	//   First child:  c(user) -> d(asst, fork)
	//                   d -> e(asst, dead-end first child)
	//                   d -> f(user) -> g(asst) -> h(user) ->
	//                        i(asst) -> j(user) -> k(asst)
	//   Second child: z(user, 1 msg)
	//
	// Old countUserTurns for c: c(user,1) -> d(asst) ->
	//   e(asst, no children) = 1 user turn <= 3 -> retry!
	// New countUserTurns for c: 1+0+1+0+1+0+1+0 = 4 > 3
	content := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID("2024-01-01T10:00:00Z", "start", "a", "").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:01Z", "ok", "b", "a").
		// First child branch from b: large subtree
		AddClaudeUserWithUUID("2024-01-01T10:00:02Z", "main1", "c", "b").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:03Z", "m-ok1", "d", "c").
		// Nested fork at d: first child is a dead-end
		AddClaudeAssistantWithUUID("2024-01-01T10:00:04Z", "dead-end", "e", "d").
		// Second child of d's fork continues the real conversation
		AddClaudeUserWithUUID("2024-01-01T10:00:05Z", "main2", "f", "d").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:06Z", "m-ok2", "g", "f").
		AddClaudeUserWithUUID("2024-01-01T10:00:07Z", "main3", "h", "g").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:08Z", "m-ok3", "i", "h").
		AddClaudeUserWithUUID("2024-01-01T10:00:09Z", "main4", "j", "i").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:10Z", "m-ok4", "k", "j").
		// Second child of b's fork: trivial retry
		AddClaudeUserWithUUID("2024-01-01T10:01:00Z", "retry", "z", "b").
		String()

	// The first child subtree has 4 user turns (c,f,h,j) > 3,
	// so it should be treated as a large-gap fork. We expect
	// 2 results: main path (a,b,c,d,f,g,h,i,j,k = 10 msgs)
	// and the fork (z = 1 msg).
	results := parseTestContent(t, "nested-fork-subtree.jsonl", content, 2)

	// Main path should follow first child at b, then second
	// child at d (the retry heuristic picks last child when
	// first child has <= 3 user turns — here "e" is a dead
	// end with 0 user turns so the nested fork follows "f").
	main := results[0]
	assert.GreaterOrEqual(t, main.Session.MessageCount, 8,
		"main MessageCount (first child subtree should not be discarded)")

	// The trivial "retry" branch should be the fork.
	fork := results[1]
	assertMessage(t, fork.Messages[0], RoleUser, "retry")
}

func TestSessionBoundsDAGMainWidenedNotFork(t *testing.T) {
	// DAG session with a trailing queue-operation after all
	// messages. Main session's EndedAt should be widened;
	// fork session should use only its own message bounds.
	queueLine := `{"type":"queue-operation","operation":"enqueue",` +
		`"timestamp":"2024-01-01T12:00:00Z","content":"{}"}`

	// Main: a->b->c->d->e->f->g->h->k->l (5 user turns)
	// Fork from b: i->j
	content := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID("2024-01-01T10:00:00Z", "hello", "a", "").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:01Z", "hi", "b", "a").
		AddClaudeUserWithUUID("2024-01-01T10:00:02Z", "q1", "c", "b").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:03Z", "a1", "d", "c").
		AddClaudeUserWithUUID("2024-01-01T10:00:04Z", "q2", "e", "d").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:05Z", "a2", "f", "e").
		AddClaudeUserWithUUID("2024-01-01T10:00:06Z", "q3", "g", "f").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:07Z", "a3", "h", "g").
		AddClaudeUserWithUUID("2024-01-01T10:00:08Z", "q4", "k", "h").
		AddClaudeAssistantWithUUID("2024-01-01T10:00:09Z", "a4", "l", "k").
		// Fork from b
		AddClaudeUserWithUUID("2024-01-01T10:01:00Z", "fork", "i", "b").
		AddClaudeAssistantWithUUID("2024-01-01T10:01:01Z", "fork-a", "j", "i").
		AddRaw(queueLine).
		String()

	results := parseTestContent(t, "dag-queue.jsonl", content, 2)

	// Main session EndedAt should be widened to queue timestamp.
	assert.Equal(t, "2024-01-01T12:00:00Z", formatTime(results[0].Session.EndedAt), "main EndedAt")

	// Fork session EndedAt should NOT be widened.
	assert.Equal(t, "2024-01-01T10:01:01Z", formatTime(results[1].Session.EndedAt), "fork EndedAt")
}
