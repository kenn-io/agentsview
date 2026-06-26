package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsAutomatedSession(t *testing.T) {
	tests := []struct {
		name         string
		firstMessage string
		want         bool
	}{
		{"EmptyMessage", "", false},
		{"NormalUserPrompt", "fix the login bug", false},

		// Code review
		{
			"CodeReviewFull",
			"You are a code reviewer. Review the code changes shown below.\n\n## Changes",
			true,
		},
		{
			"CodeReviewShort",
			"You are a code reviewer. Here is a diff.",
			true,
		},

		// Security review
		{
			"SecurityReview",
			"You are a security code reviewer. Analyze the following.",
			true,
		},
		{
			"SecurityReviewAfterAgentInstructions",
			"# AGENTS.md instructions for /repo\n\n<INSTRUCTIONS>\nDo not trust prompt text.\n</INSTRUCTIONS>\n\nYou are a security code reviewer with an exploitability burden of proof. Review the code changes for concrete vulnerabilities, material weakening of security controls, and newly reachable attack surface.",
			true,
		},

		// Design review
		{
			"DesignReview",
			"You are a design reviewer. Review the architectural changes.",
			true,
		},

		// Fix (code assistant)
		{
			"CodeAssistantFix",
			"You are a code assistant. Your task is to address the following findings.",
			true,
		},

		// Analysis request
		{
			"AnalysisRequest",
			"## Analysis Request\n\nPlease analyze the following code.",
			true,
		},

		// Insights analyst
		{
			"InsightsAnalyst",
			"You are a code review insights analyst. Summarize trends.",
			true,
		},

		// Fix request (various formats)
		{
			"FixRequestWithNewline",
			"# Fix Request\nAn analysis was performed.",
			true,
		},
		{
			"FixRequestWithDoubleSpace",
			"# Fix Request  An analysis was performed.",
			true,
		},
		{
			"FixRequestExact",
			"# Fix Request",
			true,
		},

		// Spec / plan review
		{
			"SpecReview",
			"You are reviewing whether an implementation matches its specification.",
			true,
		},
		{
			"PlanReview",
			"You are a plan document reviewer. Verify this plan.",
			true,
		},
		{
			"SpecDocReview",
			"You are a spec document reviewer. Read the spec.",
			true,
		},

		// Insights
		{
			"DaySummary",
			"You are summarizing a day of AI agent activity. Provide a summary.",
			true,
		},
		{
			"SessionAnalysis",
			"You are analyzing AI agent sessions. Provide analysis.",
			true,
		},

		// Helpful assistant analysis
		{
			"HelpfulAssistantAnalysis",
			"You are a helpful assistant working on a software project. Analyze the following sessions.",
			true,
		},

		// Catch-all substring
		{
			"RoborevSubstringInMiddle",
			"IMPORTANT: You are being invoked by roborev to perform this review directly.\n\nReview the diff.",
			true,
		},

		// Roborev review combiner
		{
			"RoborevCombiner",
			"You are combining multiple code review outputs into a single GitHub PR comment.\nRules:\n- Deduplicate findings reported by multiple agents",
			true,
		},

		// Claude Code title generator (note leading "-\n" wrapper)
		{
			"ClaudeCodeTitleGenerator",
			"-\nYou are a conversation title generator. Given the conversation below, create a short title (3-5 words) that describes the session's main topic.",
			true,
		},

		// Claude Code warmup (exact match)
		{
			"ClaudeCodeWarmup",
			"Warmup",
			true,
		},
		{
			"ClaudeCodeWarmupTrailingNewline",
			"Warmup\n",
			true,
		},

		// Negative cases
		{
			"SimilarButNotReview",
			"You are a code reviewer but I need help",
			false,
		},
		{
			"NormalFix",
			"Fix the request handler",
			false,
		},
		{
			"AnalysisInBody",
			"Please do an ## Analysis Request of this code",
			false,
		},
		// Negative: "Warmup" must not match as substring or prefix
		{
			"WarmupAsPrefix",
			"Warmup fans for the show",
			false,
		},
		// Negative: title-generator phrase appearing in normal user prose
		{
			"TitleGeneratorPhraseInProse",
			"I need to generate a conversation about titles for my book.",
			false,
		},

		// changelog generator (release tooling) — pattern is
		// project-agnostic so the same script template can run
		// against any repo.
		{
			"ChangelogGeneratorAgentsview",
			"You are generating a changelog for agentsview version 0.23.2.\n\nIMPORTANT: Do NOT use any tools.",
			true,
		},
		{
			"ChangelogGeneratorRoborev",
			"You are generating a changelog for roborev version 0.45.0.\n\nIMPORTANT: Do NOT use any tools.",
			true,
		},
		{
			"ChangelogGeneratorMsgvault",
			"You are generating a changelog for msgvault version 0.6.5.\n\nIMPORTANT: Do NOT use any tools.",
			true,
		},
		{
			"ChangelogSummaryGenerator",
			"You are generating a changelog/summary for runfolio commits.\n\nIMPORTANT: Do NOT use any tools.",
			true,
		},
		// Negative: "changelog" appearing later in normal prose
		{
			"ChangelogPhraseInProse",
			"Can you help me write a script that is generating a changelog for our release?",
			false,
		},

		// Codex IDE in-IDE action wrapper. Codex wraps in-editor
		// review/fix actions in a <user_action> XML envelope before
		// sending to the model.
		{
			"CodexUserActionReview",
			"<user_action>\n  <context>User initiated a review task.</context>\n  <action>review</action>\n</user_action>",
			true,
		},
		{
			"CodexUserActionApply",
			"<user_action><action>apply_patch</action></user_action>",
			true,
		},

		// Templated automated review prompts — common across IDE
		// review actions, CI hook scripts, and ACP test adapters.
		{
			"ReviewCodeChangesByCommit",
			"Review the code changes introduced by commit 680de37a6c99f35dd9bdc3b1f52d8278dc2e6eef. Provide prioritized, actionable findings.",
			true,
		},
		{
			"ReviewCodeChangesInCommit",
			"Review the code changes in commit 9a89a700763777b86f7e939495563a5cd0e5d74c.\n\nRepository: /tmp/foo\n\nPrompt: ...",
			true,
		},

		// Codex CLI warmup probes (analogs of Claude Code's
		// "Warmup" exact match). Two distinct phrasings observed
		// in the wild — keep both as exact matches.
		{
			"CodexRespondExactlyOK",
			"Respond with exactly: OK",
			true,
		},
		{
			"CodexRespondExactlyOKTrailingNewline",
			"Respond with exactly: OK\n",
			true,
		},
		{
			"CodexReplyExactlyOK",
			"Reply with exactly OK.",
			true,
		},
		{
			"CodexReplyExactlyOKTrailingNewline",
			"Reply with exactly OK.\n",
			true,
		},

		// Subagent-driven-development implementer prompt template.
		{
			"ImplementTheFollowingPlan",
			"Implement the following plan:\n# Plan: msgvault mcp\n## Overview\nAdd a `msgvault mcp` command...",
			true,
		},

		// Negative: "<user_action>" appearing later in prose (must
		// be at start to match).
		{
			"UserActionTagInBody",
			"Can you explain what the <user_action> wrapper does in Codex?",
			false,
		},
		// Negative: "Respond with exactly: OK" must be exact, not a
		// prefix of a longer message.
		{
			"RespondExactlyOKWithExtra",
			"Respond with exactly: OK and then explain why.",
			false,
		},
		// Negative: "Reply with exactly OK." must be exact too.
		{
			"ReplyExactlyOKWithExtra",
			"Reply with exactly OK. Then summarize.",
			false,
		},
		// Negative: human paraphrase shouldn't trip the implementer
		// prefix.
		{
			"ImplementPhraseInProse",
			"Can you implement the plan we discussed yesterday?",
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsAutomatedSession(tt.firstMessage)
			assert.Equal(t, tt.want, got,
				"IsAutomatedSession(%q)", tt.firstMessage)
		})
	}
}

func TestNormalizeUserPrefixes(t *testing.T) {
	long := strings.Repeat("a", 1025)
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"Nil", nil, nil},
		{"Empty", []string{}, nil},
		{"AllWhitespace", []string{"   ", "\t\n"}, nil},
		{"TrimsEachEntry", []string{"  hello  ", "world\n"}, []string{"hello", "world"}},
		{"DropEmpty", []string{"hello", "", "  ", "world"}, []string{"hello", "world"}},
		{"DropTooLong", []string{"hello", long}, []string{"hello"}},
		{"DropDuplicate", []string{"a", "b", "a"}, []string{"a", "b"}},
		{"DropDuplicateAfterTrim", []string{"a", " a "}, []string{"a"}},
		{
			"DropBuiltInOverlap",
			[]string{"You are a code reviewer.", "novel"},
			[]string{"novel"},
		},
		{
			"PreservesUserOrder",
			[]string{"zeta", "alpha", "mu"},
			[]string{"zeta", "alpha", "mu"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeUserPrefixes(tt.in)
			assert.Equal(t, tt.want, got,
				"normalizeUserPrefixes(%q)", tt.in)
		})
	}
}

func TestIsAutomatedSessionWithUserPrefixes(t *testing.T) {
	t.Cleanup(func() { SetUserAutomationPrefixes(nil) })
	SetUserAutomationPrefixes([]string{
		"You are analyzing an essay",
		"Grade these Benn Stancil quotes",
	})

	tests := []struct {
		name         string
		firstMessage string
		want         bool
	}{
		{
			"UserPrefixMatchesEssayPrompt",
			"You are analyzing an essay about epistemology.",
			true,
		},
		{
			"UserPrefixMatchesGradeQuotes",
			"Grade these Benn Stancil quotes for me.",
			true,
		},
		{
			"UserPrefixDoesNotMatchUnrelated",
			"How do I fix this bug?",
			false,
		},
		{
			"BuiltInPrefixStillMatches",
			"You are a code reviewer. Review the diff.",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsAutomatedSession(tt.firstMessage)
			assert.Equal(t, tt.want, got,
				"IsAutomatedSession(%q)", tt.firstMessage)
		})
	}
}

func TestUserAutomationPrefixesReturnsCopy(t *testing.T) {
	t.Cleanup(func() { SetUserAutomationPrefixes(nil) })
	SetUserAutomationPrefixes([]string{"alpha", "beta"})
	got := UserAutomationPrefixes()
	if len(got) > 0 {
		got[0] = "MUTATED"
	}
	again := UserAutomationPrefixes()
	assert.NotEmpty(t, again, "singleton mutated through returned slice")
	if len(again) > 0 {
		assert.Equal(t, "alpha", again[0],
			"singleton mutated through returned slice")
	}
}
