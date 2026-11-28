package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// fingerprint summarizes one runtime session's catalog metadata. Sessions
// whose rows did not change keep the same fingerprint across catalog writes.
func (c *codebuddyCatalog) fingerprint(runtimeID string) string {
	if c == nil {
		return "absent"
	}
	e := c.byRuntime[runtimeID]
	if e == nil {
		return "none"
	}
	h := sha256.New()
	// One parent runtime id hashes as before; more change the hash when a
	// mapping the parent gains or loses can move the fork link.
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00", e.conversationID,
		e.title, e.customTitle, strings.Join(e.forkParentRuntimeIDs, "\x01"))
	for _, entry := range e.btw {
		codebuddyHashEntry(h, entry)
	}
	for _, thread := range e.discussions {
		_, _ = fmt.Fprintf(h, "thread\x00%s\x00%s\x00", thread.id, thread.quote)
		for _, entry := range thread.entries {
			codebuddyHashEntry(h, entry)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func codebuddyHashEntry(h interface{ Write([]byte) (int, error) }, e codebuddyBtwEntry) {
	_, _ = fmt.Fprintf(h, "entry\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00",
		e.id, e.parentID, e.question, e.answer, e.status, e.errText,
		e.imageCount, e.updatedAt.UnixMilli())
}

// codebuddySideQuestionResults turns /btw entries and discussion threads into
// child sessions of the conversation's main session. Each side question is
// asked outside the main transcript, so it stays out of the main tool order.
func codebuddySideQuestionResults(
	results []ParseResult, entry *codebuddyCatalogEntry,
) []ParseResult {
	if len(results) == 0 || entry == nil {
		return nil
	}
	main := results[0].Session
	if main.RelationshipType == RelSubagent {
		return nil
	}
	if main.RelationshipType == RelFork && main.SourceSessionID == "" &&
		main.ParentSessionID != "" {
		// A conversation rewound before its first message returns only its
		// preserved branches; side questions still belong to the
		// conversation's own session.
		main.ID = main.ParentSessionID
	}
	var out []ParseResult
	if len(entry.btw) > 0 {
		out = append(out, codebuddySideSession(main, "btw", "btw",
			"/btw", "", entry.btw))
	}
	for _, thread := range entry.discussions {
		out = append(out, codebuddySideSession(main, "discussion",
			"discussion:"+thread.id, "Discussion", thread.quote, thread.entries))
	}
	return out
}

// codebuddyJoinStatus appends a side question's failure or cancellation to
// the answer CodeBuddy saved before it stopped, so a partial answer does not
// read as a finished one.
func codebuddyJoinStatus(answer, status string) string {
	if answer == "" {
		return status
	}
	return answer + "\n\n" + status
}

func codebuddySideSession(
	main ParsedSession, kind, suffix, label, quote string,
	entries []codebuddyBtwEntry,
) ParseResult {
	ordered := append([]codebuddyBtwEntry(nil), entries...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].createdAt.Before(ordered[j].createdAt)
	})
	// A side thread shares the conversation's place and source file but none
	// of its counts: its own messages carry no usage.
	sess := ParsedSession{
		ID: main.ID + ":" + suffix, Project: main.Project, Machine: main.Machine,
		Agent: main.Agent, Entrypoint: main.Entrypoint, Cwd: main.Cwd,
		File: main.File, ParentSessionID: main.ID, RelationshipType: RelFork,
		SessionKind: kind,
	}
	var msgs []ParsedMessage
	if quote != "" {
		text := "> " + strings.ReplaceAll(strings.TrimSpace(quote), "\n", "\n> ")
		msgs = append(msgs, ParsedMessage{
			Role: RoleUser, Content: text, IsSystem: true,
			ContentLength: len(text), SourceType: "system",
			SourceSubtype: "discussion_anchor", Timestamp: ordered[0].createdAt,
			SourceUUID: suffix + ":anchor",
		})
	}
	var started, ended time.Time
	users := 0
	for _, e := range ordered {
		if started.IsZero() || (!e.createdAt.IsZero() && e.createdAt.Before(started)) {
			started = e.createdAt
		}
		ended = laterTime(ended, laterTime(e.createdAt, e.updatedAt))
		question := e.question
		if e.imageCount > 0 {
			question += "\n\n[" + strconv.Itoa(e.imageCount) + " image(s) attached; CodeBuddy did not store them]"
		}
		msgs = append(msgs, ParsedMessage{
			Role: RoleUser, Content: question, DialogueText: new(question),
			ContentLength: len(question), Timestamp: e.createdAt,
			SourceType: "user", SourceUUID: e.id, SourceParentUUID: e.parentID,
		})
		users++
		answer := e.answer
		switch {
		case e.status == "error":
			answer = codebuddyJoinStatus(answer,
				"[Side question failed] "+firstNonEmptyJSONLString(e.errText, "no answer was recorded"))
		case e.status == "cancelled":
			answer = codebuddyJoinStatus(answer, "[Side question cancelled]")
		case answer != "":
		default:
			answer = "[No answer was recorded]"
		}
		msg := ParsedMessage{
			Role: RoleAssistant, Content: answer, ContentLength: len(answer),
			Timestamp: laterTime(e.createdAt, e.updatedAt), SourceUUID: e.id + ":answer",
			SourceParentUUID: e.id,
		}
		if e.answer != "" {
			msg.DialogueText = new(answer)
		} else {
			msg.IsSystem = true
			msg.SourceType = "system"
			msg.SourceSubtype = "interrupted"
		}
		msgs = append(msgs, msg)
	}
	for i := range msgs {
		msgs[i].Ordinal = i
	}
	sess.StartedAt = started
	sess.EndedAt = ended
	sess.MessageCount = len(msgs)
	sess.UserMessageCount = users
	sess.FirstMessage = truncate(strings.ReplaceAll(ordered[0].question, "\n", " "), 300)
	sess.SessionName = label + ": " + truncate(ordered[0].question, 60)
	accumulateMessageTokenUsage(&sess, msgs)
	return ParseResult{Session: sess, Messages: msgs}
}
