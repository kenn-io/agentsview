package nanoclaw

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUnwrapEnvelope(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "unwrap_envelope_forms/batched_messages_join_with_newlines",
			in: "<context tz=\"x\" />\n" +
				"<message id=\"1\" from=\"mg\">first line</message>\n" +
				"<message id=\"2\" from=\"mg\">second &amp; third</message>",
			want: "first line\nsecond & third",
		},
		{
			name: "unwrap_envelope_forms/no_envelope_is_trimmed_as_is",
			in:   "  plain prompt  ",
			want: "plain prompt",
		},
		{
			name: "unwrap_envelope_forms/empty_message_body",
			in:   `<message id="1"></message>`,
			want: "",
		},
		{
			name: "nanoclaw_load_unwraps_envelope_and_maps_tool_results/cell_envelope",
			in: "<context timezone=\"UTC\" />\n<message id=\"2\" from=\"chat-mg-1\" " +
				"sender=\"100@example.invalid\" time=\"Jul 8, 2026, 4:11 PM\">" +
				"no helper, don&#39;t answer in that channel</message>",
			want: "no helper, don't answer in that channel",
		},
		{
			name: "amp_is_unescaped_last",
			in:   "<message>&amp;lt;b&amp;gt;</message>",
			want: "&lt;b&gt;",
		},
		{
			name: "all_entities",
			in:   "<message>&lt;&gt;&quot;&#39;&apos;&amp;</message>",
			want: `<>"''&`,
		},
		{
			name: "body_spans_lines",
			in:   "<message id=\"1\">line one\nline two</message>",
			want: "line one\nline two",
		},
		{
			name: "empty_parts_are_dropped",
			in:   "<message> </message><message>kept</message>",
			want: "kept",
		},
		{
			name: "word_boundary_after_tag_name",
			in:   " <messages>x</messages> ",
			want: "<messages>x</messages>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, UnwrapEnvelope(tt.in))
		})
	}
}
