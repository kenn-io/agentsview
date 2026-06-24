package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeChatGPTFixture(
	t *testing.T, dir, filename, content string,
) {
	t.Helper()
	err := os.WriteFile(
		filepath.Join(dir, filename),
		[]byte(content),
		0o644,
	)
	require.NoError(t, err)
}

// Basic 3-message conversation: user, assistant, user.
func TestParseChatGPTExport(t *testing.T) {
	dir := t.TempDir()
	writeChatGPTFixture(t, dir, "conversations-001.json", `[
  {
    "conversation_id": "abc-123",
    "title": "Hello Chat",
    "create_time": 1700000000.0,
    "update_time": 1700000060.0,
    "current_node": "node-3",
    "mapping": {
      "root": {
        "id": "root",
        "parent": null,
        "children": ["node-1"],
        "message": null
      },
      "node-1": {
        "id": "node-1",
        "parent": "root",
        "children": ["node-2"],
        "message": {
          "id": "msg-1",
          "create_time": 1700000010.0,
          "author": {"role": "user"},
          "content": {
            "content_type": "text",
            "parts": ["What is Go?"]
          },
          "status": "finished_successfully",
          "metadata": {}
        }
      },
      "node-2": {
        "id": "node-2",
        "parent": "node-1",
        "children": ["node-3"],
        "message": {
          "id": "msg-2",
          "create_time": 1700000020.0,
          "author": {"role": "assistant"},
          "content": {
            "content_type": "text",
            "parts": ["Go is a programming language."]
          },
          "status": "finished_successfully",
          "metadata": {"model_slug": "gpt-4"}
        }
      },
      "node-3": {
        "id": "node-3",
        "parent": "node-2",
        "children": [],
        "message": {
          "id": "msg-3",
          "create_time": 1700000050.0,
          "author": {"role": "user"},
          "content": {
            "content_type": "text",
            "parts": ["Thanks!"]
          },
          "status": "finished_successfully",
          "metadata": {}
        }
      }
    }
  }
]`)

	var results []ParseResult
	err := parseChatGPTExport(dir, nil, func(r ParseResult) error {
		results = append(results, r)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 1)

	s := results[0].Session
	assert.Equal(t, "chatgpt:abc-123", s.ID)
	assert.Equal(t, "chatgpt.com", s.Project)
	assert.Equal(t, "local", s.Machine)
	assert.Equal(t, AgentChatGPT, s.Agent)
	assert.Equal(t, "Hello Chat", s.SessionName)
	assert.Equal(t, "What is Go?", s.FirstMessage)
	assert.Equal(t, 3, s.MessageCount)
	assert.Equal(t, 2, s.UserMessageCount)

	msgs := results[0].Messages
	require.Len(t, msgs, 3)

	assert.Equal(t, 0, msgs[0].Ordinal)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "What is Go?", msgs[0].Content)

	assert.Equal(t, 1, msgs[1].Ordinal)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, "Go is a programming language.", msgs[1].Content)
	assert.Equal(t, "gpt-4", msgs[1].Model)

	assert.Equal(t, 2, msgs[2].Ordinal)
	assert.Equal(t, RoleUser, msgs[2].Role)
	assert.Equal(t, "Thanks!", msgs[2].Content)
}

// Tool nodes (code_interpreter + execution_output) should
// be attached to the preceding assistant message.
func TestParseChatGPTExport_ToolCalls(t *testing.T) {
	dir := t.TempDir()
	writeChatGPTFixture(t, dir, "conversations-001.json", `[
  {
    "conversation_id": "tool-conv",
    "title": "Code Run",
    "create_time": 1700000000.0,
    "update_time": 1700000060.0,
    "current_node": "node-4",
    "mapping": {
      "root": {
        "id": "root",
        "parent": null,
        "children": ["node-1"],
        "message": null
      },
      "node-1": {
        "id": "node-1",
        "parent": "root",
        "children": ["node-2"],
        "message": {
          "id": "msg-1",
          "create_time": 1700000010.0,
          "author": {"role": "user"},
          "content": {
            "content_type": "text",
            "parts": ["Run print(42)"]
          },
          "status": "finished_successfully",
          "metadata": {}
        }
      },
      "node-2": {
        "id": "node-2",
        "parent": "node-1",
        "children": ["node-3"],
        "message": {
          "id": "msg-2",
          "create_time": 1700000020.0,
          "author": {"role": "assistant"},
          "content": {
            "content_type": "text",
            "parts": ["Let me run that for you."]
          },
          "status": "finished_successfully",
          "metadata": {"model_slug": "gpt-4"}
        }
      },
      "node-3": {
        "id": "node-3",
        "parent": "node-2",
        "children": ["node-4"],
        "message": {
          "id": "msg-code",
          "create_time": 1700000030.0,
          "author": {"role": "tool", "name": "python"},
          "content": {
            "content_type": "code",
            "text": "print(42)",
            "language": "python"
          },
          "status": "finished_successfully",
          "metadata": {}
        }
      },
      "node-4": {
        "id": "node-4",
        "parent": "node-3",
        "children": [],
        "message": {
          "id": "msg-output",
          "create_time": 1700000035.0,
          "author": {"role": "tool", "name": "python"},
          "content": {
            "content_type": "execution_output",
            "text": "42"
          },
          "status": "finished_successfully",
          "metadata": {}
        }
      }
    }
  }
]`)

	var results []ParseResult
	err := parseChatGPTExport(dir, nil, func(r ParseResult) error {
		results = append(results, r)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 1)

	msgs := results[0].Messages
	// Tool nodes are NOT separate messages.
	require.Len(t, msgs, 2, "tool nodes should be attached, not separate")

	asst := msgs[1]
	assert.Equal(t, RoleAssistant, asst.Role)
	assert.True(t, asst.HasToolUse)
	require.Len(t, asst.ToolCalls, 1)
	assert.Equal(t, "code_interpreter", asst.ToolCalls[0].ToolName)
	assert.Equal(t, "Bash", asst.ToolCalls[0].Category)

	// execution_output is paired as a result event.
	require.Len(t, asst.ToolCalls[0].ResultEvents, 1)
	assert.Contains(t, asst.ToolCalls[0].ResultEvents[0].Content, "42")
}

// Thoughts content type should produce [Thinking] blocks.
func TestParseChatGPTExport_Thinking(t *testing.T) {
	dir := t.TempDir()
	writeChatGPTFixture(t, dir, "conversations-001.json", `[
  {
    "conversation_id": "think-conv",
    "title": "Thinking Test",
    "create_time": 1700000000.0,
    "update_time": 1700000060.0,
    "current_node": "node-2",
    "mapping": {
      "root": {
        "id": "root",
        "parent": null,
        "children": ["node-1"],
        "message": null
      },
      "node-1": {
        "id": "node-1",
        "parent": "root",
        "children": ["node-2"],
        "message": {
          "id": "msg-1",
          "create_time": 1700000010.0,
          "author": {"role": "user"},
          "content": {
            "content_type": "text",
            "parts": ["Explain recursion"]
          },
          "status": "finished_successfully",
          "metadata": {}
        }
      },
      "node-2": {
        "id": "node-2",
        "parent": "node-1",
        "children": [],
        "message": {
          "id": "msg-2",
          "create_time": 1700000020.0,
          "author": {"role": "assistant"},
          "content": {
            "content_type": "thoughts",
            "thoughts": [
              {"content": "Let me think about recursion."},
              {"content": "It is a function calling itself."}
            ]
          },
          "status": "finished_successfully",
          "metadata": {"model_slug": "o1-preview"}
        }
      }
    }
  }
]`)

	var results []ParseResult
	err := parseChatGPTExport(dir, nil, func(r ParseResult) error {
		results = append(results, r)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 1)

	msgs := results[0].Messages
	require.Len(t, msgs, 2)

	asst := msgs[1]
	assert.True(t, asst.HasThinking)
	assert.Contains(t, asst.Content, "[Thinking]")
	assert.Contains(t, asst.Content, "Let me think about recursion.")
	assert.Contains(t, asst.Content, "It is a function calling itself.")
	assert.Contains(t, asst.Content, "[/Thinking]")
	assert.Equal(t, "o1-preview", asst.Model)
}

// System nodes should have IsSystem = true and count in
// MessageCount.
func TestParseChatGPTExport_SystemMessage(t *testing.T) {
	dir := t.TempDir()
	writeChatGPTFixture(t, dir, "conversations-001.json", `[
  {
    "conversation_id": "sys-conv",
    "title": "System Test",
    "create_time": 1700000000.0,
    "update_time": 1700000060.0,
    "current_node": "node-2",
    "mapping": {
      "root": {
        "id": "root",
        "parent": null,
        "children": ["node-sys"],
        "message": null
      },
      "node-sys": {
        "id": "node-sys",
        "parent": "root",
        "children": ["node-1"],
        "message": {
          "id": "msg-sys",
          "create_time": 1700000005.0,
          "author": {"role": "system"},
          "content": {
            "content_type": "text",
            "parts": ["You are a helpful assistant."]
          },
          "status": "finished_successfully",
          "metadata": {}
        }
      },
      "node-1": {
        "id": "node-1",
        "parent": "node-sys",
        "children": ["node-2"],
        "message": {
          "id": "msg-1",
          "create_time": 1700000010.0,
          "author": {"role": "user"},
          "content": {
            "content_type": "text",
            "parts": ["Hi"]
          },
          "status": "finished_successfully",
          "metadata": {}
        }
      },
      "node-2": {
        "id": "node-2",
        "parent": "node-1",
        "children": [],
        "message": {
          "id": "msg-2",
          "create_time": 1700000020.0,
          "author": {"role": "assistant"},
          "content": {
            "content_type": "text",
            "parts": ["Hello!"]
          },
          "status": "finished_successfully",
          "metadata": {"model_slug": "gpt-4"}
        }
      }
    }
  }
]`)

	var results []ParseResult
	err := parseChatGPTExport(dir, nil, func(r ParseResult) error {
		results = append(results, r)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 1)

	s := results[0].Session
	assert.Equal(t, 3, s.MessageCount)
	assert.Equal(t, 1, s.UserMessageCount)

	msgs := results[0].Messages
	require.Len(t, msgs, 3)

	assert.True(t, msgs[0].IsSystem)
	assert.Equal(t, "You are a helpful assistant.", msgs[0].Content)
	assert.False(t, msgs[1].IsSystem)
	assert.Equal(t, RoleUser, msgs[1].Role)
}

// Empty directory should produce no results and no error.
func TestParseChatGPTExport_EmptyDir(t *testing.T) {
	dir := t.TempDir()

	err := parseChatGPTExport(dir, nil, func(r ParseResult) error {
		return nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no conversation files")
}

// Multiple conversation files should all be processed.
func TestParseChatGPTExport_MultipleShards(t *testing.T) {
	dir := t.TempDir()

	shard := func(id, title string) string {
		return `[{
      "conversation_id": "` + id + `",
      "title": "` + title + `",
      "create_time": 1700000000.0,
      "update_time": 1700000060.0,
      "current_node": "node-1",
      "mapping": {
        "root": {
          "id": "root",
          "parent": null,
          "children": ["node-1"],
          "message": null
        },
        "node-1": {
          "id": "node-1",
          "parent": "root",
          "children": [],
          "message": {
            "id": "msg-1",
            "create_time": 1700000010.0,
            "author": {"role": "user"},
            "content": {
              "content_type": "text",
              "parts": ["Hello"]
            },
            "status": "finished_successfully",
            "metadata": {}
          }
        }
      }
    }]`
	}

	writeChatGPTFixture(t, dir, "conversations-001.json",
		shard("conv-a", "First"))
	writeChatGPTFixture(t, dir, "conversations-002.json",
		shard("conv-b", "Second"))

	var results []ParseResult
	err := parseChatGPTExport(dir, nil, func(r ParseResult) error {
		results = append(results, r)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 2)

	assert.Equal(t, "chatgpt:conv-a", results[0].Session.ID)
	assert.Equal(t, "chatgpt:conv-b", results[1].Session.ID)
}

// Verify DAG linearization walks from current_node to root and
// reverses.
func TestLinearizeDAG(t *testing.T) {
	parent := "root"
	mapping := map[string]chatGPTNode{
		"root": {
			ID:       "root",
			Parent:   nil,
			Children: []string{"a"},
		},
		"a": {
			ID:       "a",
			Parent:   &parent,
			Children: []string{"b"},
		},
		"b": {
			ID:     "b",
			Parent: new("a"),
		},
	}

	nodes := linearizeDAG(mapping, "b")
	require.Len(t, nodes, 3)
	assert.Equal(t, "root", nodes[0].ID)
	assert.Equal(t, "a", nodes[1].ID)
	assert.Equal(t, "b", nodes[2].ID)
}

func TestLinearizeDAG_EmptyCurrentNode(t *testing.T) {
	nodes := linearizeDAG(map[string]chatGPTNode{}, "")
	assert.Nil(t, nodes)
}

func TestLinearizeDAG_MissingNode(t *testing.T) {
	nodes := linearizeDAG(map[string]chatGPTNode{}, "missing")
	assert.Empty(t, nodes)
}

// Verify assembleContent handles all content types.
func TestAssembleContent(t *testing.T) {
	tests := []struct {
		name string
		c    chatGPTContent
		want string
	}{
		{
			name: "text with string parts",
			c: chatGPTContent{
				ContentType: "text",
				Parts:       rawParts("Hello", "World"),
			},
			want: "Hello\nWorld",
		},
		{
			name: "code block",
			c: chatGPTContent{
				ContentType: "code",
				Text:        "print(42)",
				Language:    "python",
			},
			want: "```python\nprint(42)\n```",
		},
		{
			name: "execution_output",
			c: chatGPTContent{
				ContentType: "execution_output",
				Text:        "42",
			},
			want: "```\n42\n```",
		},
		{
			name: "thoughts",
			c: chatGPTContent{
				ContentType: "thoughts",
				Thoughts: []chatGPTThought{
					{Content: "thinking hard"},
				},
			},
			want: "[Thinking]\nthinking hard\n[/Thinking]",
		},
		{
			name: "tether_quote",
			c: chatGPTContent{
				ContentType: "tether_quote",
				Text:        "some quote",
				Title:       "Source",
				URL:         "https://example.com",
			},
			want: "> some quote\n> -- [Source](https://example.com)",
		},
		{
			name: "reasoning_recap is skipped",
			c: chatGPTContent{
				ContentType: "reasoning_recap",
				Text:        "recap text",
			},
			want: "",
		},
		{
			name: "tether_browsing_display is skipped",
			c: chatGPTContent{
				ContentType: "tether_browsing_display",
			},
			want: "",
		},
		{
			name: "system_error",
			c: chatGPTContent{
				ContentType: "system_error",
				Text:        "something went wrong",
			},
			want: "something went wrong",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := assembleContent(tt.c, nil)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Verify image asset resolution with a mock resolver.
func TestAssembleContent_ImageAsset(t *testing.T) {
	c := chatGPTContent{
		ContentType: "multimodal_text",
		Parts: []json.RawMessage{
			json.RawMessage(`"Here is an image:"`),
			json.RawMessage(`{
				"content_type": "image_asset_pointer",
				"asset_pointer": "file-service://file-abc123"
			}`),
		},
	}

	resolver := &mockAssetResolver{
		files: map[string]string{
			"file-service://file-abc123": "/tmp/img.png",
		},
		copyRef: "asset://abc123.png",
	}

	got := assembleContent(c, resolver)
	assert.Contains(t, got, "Here is an image:")
	assert.Contains(t, got, "![image](asset://abc123.png)")
}

// Nil resolver should produce [image unavailable].
func TestAssembleContent_ImageAsset_NilResolver(t *testing.T) {
	c := chatGPTContent{
		ContentType: "multimodal_text",
		Parts: []json.RawMessage{
			json.RawMessage(`{
				"content_type": "image_asset_pointer",
				"asset_pointer": "file-service://file-abc123"
			}`),
		},
	}

	got := assembleContent(c, nil)
	assert.Equal(t, "[image unavailable]", got)
}

func TestUnixFloatToTime(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		assert.True(t, unixFloatToTime(nil).IsZero())
	})

	t.Run("zero", func(t *testing.T) {
		z := 0.0
		assert.True(t, unixFloatToTime(&z).IsZero())
	})

	t.Run("valid", func(t *testing.T) {
		v := 1700000000.5
		got := unixFloatToTime(&v)
		assert.Equal(t, int64(1700000000), got.Unix())
		assert.Equal(t, 500000000, got.Nanosecond())
	})
}

// Conversation with web_search tool nodes.
func TestParseChatGPTExport_WebSearch(t *testing.T) {
	dir := t.TempDir()
	writeChatGPTFixture(t, dir, "conversations-001.json", `[
  {
    "conversation_id": "web-conv",
    "title": "Web Search",
    "create_time": 1700000000.0,
    "update_time": 1700000060.0,
    "current_node": "node-3",
    "mapping": {
      "root": {
        "id": "root",
        "parent": null,
        "children": ["node-1"],
        "message": null
      },
      "node-1": {
        "id": "node-1",
        "parent": "root",
        "children": ["node-2"],
        "message": {
          "id": "msg-1",
          "create_time": 1700000010.0,
          "author": {"role": "user"},
          "content": {
            "content_type": "text",
            "parts": ["Search for Go"]
          },
          "status": "finished_successfully",
          "metadata": {}
        }
      },
      "node-2": {
        "id": "node-2",
        "parent": "node-1",
        "children": ["node-3"],
        "message": {
          "id": "msg-2",
          "create_time": 1700000020.0,
          "author": {"role": "assistant"},
          "content": {
            "content_type": "text",
            "parts": ["Searching..."]
          },
          "status": "finished_successfully",
          "metadata": {"model_slug": "gpt-4"}
        }
      },
      "node-3": {
        "id": "node-3",
        "parent": "node-2",
        "children": [],
        "message": {
          "id": "msg-browse",
          "create_time": 1700000030.0,
          "author": {"role": "tool"},
          "content": {
            "content_type": "tether_quote",
            "text": "Go is great",
            "title": "Go Blog",
            "url": "https://go.dev/blog"
          },
          "status": "finished_successfully",
          "metadata": {}
        }
      }
    }
  }
]`)

	var results []ParseResult
	err := parseChatGPTExport(dir, nil, func(r ParseResult) error {
		results = append(results, r)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 1)

	msgs := results[0].Messages
	require.Len(t, msgs, 2)

	asst := msgs[1]
	assert.True(t, asst.HasToolUse)
	require.Len(t, asst.ToolCalls, 1)
	assert.Equal(t, "web_search", asst.ToolCalls[0].ToolName)
	assert.Equal(t, "Tool", asst.ToolCalls[0].Category)
}

// --- helpers ---

// rawParts creates json.RawMessage slices from strings for
// chatGPTContent.Parts test fixtures.
func rawParts(ss ...string) []json.RawMessage {
	var parts []json.RawMessage
	for _, s := range ss {
		b, _ := json.Marshal(s)
		parts = append(parts, b)
	}
	return parts
}

type mockAssetResolver struct {
	files   map[string]string
	copyRef string
}

func (m *mockAssetResolver) Resolve(
	pointer string,
) (string, bool) {
	p, ok := m.files[pointer]
	return p, ok
}

func (m *mockAssetResolver) Copy(string) (string, error) {
	return m.copyRef, nil
}
