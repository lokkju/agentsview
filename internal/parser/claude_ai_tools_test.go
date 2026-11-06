package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readClaudeAIToolsDetail loads the reconstructed browser-sync detail shared
// with the importer tests. Its visible branch ends at "answer"; a sibling
// branch from "draft" ends at "rhymed".
func readClaudeAIToolsDetail(t *testing.T, leaf string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(
		"..", "importer", "testdata", "claude_ai_sync", "detail_tools.json",
	))
	require.NoError(t, err)
	if leaf == "" {
		return data
	}
	return []byte(strings.Replace(string(data),
		`"current_leaf_message_uuid": "answer"`,
		`"current_leaf_message_uuid": "`+leaf+`"`, 1))
}

type claudeAICallView struct {
	ID, Name, Category, Input, Result, Status string
}

func claudeAICallViews(calls []ParsedToolCall) []claudeAICallView {
	views := make([]claudeAICallView, 0, len(calls))
	for _, c := range calls {
		v := claudeAICallView{ID: c.ToolUseID, Name: c.ToolName, Category: c.Category, Input: c.InputJSON}
		if len(c.ResultEvents) > 0 {
			v.Result = c.ResultEvents[0].Content
			v.Status = c.ResultEvents[0].Status
		}
		views = append(views, v)
	}
	return views
}

func TestParseClaudeAIDetail_ToolCallsAttachToTheirMessage(t *testing.T) {
	result, err := ParseClaudeAIDetail(readClaudeAIToolsDetail(t, ""))
	require.NoError(t, err)

	// The text and ordinals are the ones the parser produced before it read
	// tool blocks, so archived rows (and pins on them) keep their identity.
	require.Len(t, result.Messages, 4)
	assert.Equal(t, 4, result.Session.MessageCount)
	assert.Equal(t, 2, result.Session.UserMessageCount)
	wantContent := []string{
		"Write a short poem about tides and check the moon phase\n\n[Attachment: notes.txt]\nSpring tide notes",
		"Let me check the sky first.\n\nThe moon is waxing gibbous.",
		"Count the words",
		"The poem has five words.",
	}
	for i, m := range result.Messages {
		assert.Equal(t, i, m.Ordinal)
		assert.Equal(t, wantContent[i], m.Content)
		assert.Equal(t, len(wantContent[i]), m.ContentLength)
	}

	assert.Empty(t, result.Messages[0].ToolCalls)
	assert.False(t, result.Messages[0].HasToolUse)
	assert.Empty(t, result.Messages[2].ToolCalls)

	assert.True(t, result.Messages[1].HasToolUse)
	assert.Equal(t, []claudeAICallView{
		{
			ID: "toolu_search", Name: "web_search", Category: NormalizeToolCategory("web_search"),
			Input:  `{"query":"moon phase today"}`,
			Result: "Lunar calendar\nhttps://example.com/moon\nThe moon is waxing gibbous.",
		},
		{
			ID: "toolu_poem", Name: "artifacts", Category: NormalizeToolCategory("artifacts"),
			Input:  `{"id":"tide-poem","command":"create","type":"text/markdown","title":"Tide Poem","content":"Silver water climbs the sand"}`,
			Result: "OK",
		},
	}, claudeAICallViews(result.Messages[1].ToolCalls))

	assert.True(t, result.Messages[3].HasToolUse)
	assert.Equal(t, []claudeAICallView{{
		ID: "toolu_count", Name: "repl", Category: NormalizeToolCategory("repl"),
		Input:  `{"code":"console.log('Silver water climbs the sand'.split(' ').length)"}`,
		Result: `{"status":"success","logs":["5"]}`,
	}}, claudeAICallViews(result.Messages[3].ToolCalls))
}

func TestParseClaudeAIDetail_OtherBranchesStayOut(t *testing.T) {
	visible, err := ParseClaudeAIDetail(readClaudeAIToolsDetail(t, ""))
	require.NoError(t, err)
	for _, m := range visible.Messages {
		assert.NotContains(t, m.Content, "rhym")
		for _, c := range m.ToolCalls {
			assert.NotEqual(t, "toolu_rhyme", c.ToolUseID)
		}
	}

	// Selecting the sibling branch keeps the shared prefix unchanged,
	// tool calls included, and only the branch's own turns differ.
	switched, err := ParseClaudeAIDetail(readClaudeAIToolsDetail(t, "rhymed"))
	require.NoError(t, err)
	require.Len(t, switched.Messages, 4)
	assert.Equal(t, visible.Messages[:2], switched.Messages[:2])
	assert.Equal(t, "Make it rhyme", switched.Messages[2].Content)
	assert.Equal(t, 2, switched.Messages[2].Ordinal)
	assert.Equal(t, "Here is a rhyming version.", switched.Messages[3].Content)
	assert.Equal(t, []claudeAICallView{{
		ID: "toolu_rhyme", Name: "artifacts", Category: NormalizeToolCategory("artifacts"),
		Input:  `{"id":"tide-poem","command":"update","old_str":"climbs the sand","new_str":"meets the land"}`,
		Result: "Edit failed", Status: "error",
	}}, claudeAICallViews(switched.Messages[3].ToolCalls))
}

func TestParseClaudeAIExport_ToolBlocks(t *testing.T) {
	for _, tt := range []struct {
		name    string
		blocks  string
		content string
		calls   []claudeAICallView
	}{
		{
			name:    "string result content",
			blocks:  `{"type":"tool_use","id":"t1","name":"artifacts","input":{"command":"create","content":"Body"}},{"type":"tool_result","tool_use_id":"t1","name":"artifacts","content":"Done"},{"type":"text","text":"Reply"}`,
			content: "Reply",
			calls:   []claudeAICallView{{ID: "t1", Name: "artifacts", Category: NormalizeToolCategory("artifacts"), Input: `{"command":"create","content":"Body"}`, Result: "Done"}},
		},
		{
			name:    "result without its call keeps its output",
			blocks:  `{"type":"tool_result","tool_use_id":"gone","name":"web_search","content":[{"type":"text","text":"Found"}]},{"type":"text","text":"Reply"}`,
			content: "Reply",
			calls:   []claudeAICallView{{ID: "gone", Name: "web_search", Category: NormalizeToolCategory("web_search"), Result: "Found"}},
		},
		{
			name:    "malformed tool blocks are skipped",
			blocks:  `{"type":"tool_use","id":"t1","name":7,"input":{}},{"type":"tool_result","tool_use_id":"t1","content":{"oops":true}},{"type":"tool_use","id":"t2","input":{}},{"type":"text","text":"Reply"}`,
			content: "Reply",
		},
		{
			name:    "tool-only reply falls back to message text",
			blocks:  `{"type":"tool_use","id":"t1","name":"repl","input":null}`,
			content: "Fallback",
			calls:   []claudeAICallView{{ID: "t1", Name: "repl", Category: NormalizeToolCategory("repl")}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := `[{"uuid":"c","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z","chat_messages":[` +
				`{"uuid":"q","sender":"human","text":"Question"},` +
				`{"uuid":"a","sender":"assistant","text":"Fallback","content":[` + tt.blocks + `]}]}]`
			var results []ParseResult
			require.NoError(t, parseClaudeAIExport(strings.NewReader(input), func(r ParseResult) error {
				results = append(results, r)
				return nil
			}))
			require.Len(t, results, 1)
			require.Len(t, results[0].Messages, 2)
			reply := results[0].Messages[1]
			assert.Equal(t, tt.content, reply.Content)
			assert.Equal(t, len(tt.calls) > 0, reply.HasToolUse)
			assert.Equal(t, tt.calls, nilIfEmptyCallViews(claudeAICallViews(reply.ToolCalls)))
		})
	}
}

func TestParseClaudeAIExport_ResultPairsWithCallInEarlierMessage(t *testing.T) {
	input := `[{"uuid":"c","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z","chat_messages":[` +
		`{"uuid":"a","sender":"assistant","content":[{"type":"text","text":"Searching"},{"type":"tool_use","id":"t1","name":"web_search","input":{"query":"q"}}]},` +
		`{"uuid":"b","sender":"assistant","content":[{"type":"tool_result","tool_use_id":"t1","name":"web_search","content":[{"type":"text","text":"Late result"}]},{"type":"text","text":"Done"}]}]}]`
	var results []ParseResult
	require.NoError(t, parseClaudeAIExport(strings.NewReader(input), func(r ParseResult) error {
		results = append(results, r)
		return nil
	}))
	require.Len(t, results, 1)
	msgs := results[0].Messages
	require.Len(t, msgs, 2)
	assert.Equal(t, []claudeAICallView{{
		ID: "t1", Name: "web_search", Category: NormalizeToolCategory("web_search"),
		Input: `{"query":"q"}`, Result: "Late result",
	}}, claudeAICallViews(msgs[0].ToolCalls))
	assert.Empty(t, msgs[1].ToolCalls)
	assert.False(t, msgs[1].HasToolUse)
}

func nilIfEmptyCallViews(views []claudeAICallView) []claudeAICallView {
	if len(views) == 0 {
		return nil
	}
	return views
}

func TestParseClaudeAIDetailWithImages(t *testing.T) {
	const ask = "Write a short poem about tides and check the moon phase\n\n[Attachment: notes.txt]\nSpring tide notes"
	for _, tt := range []struct {
		name    string
		ref     string
		ok      bool
		wantAsk string
	}{
		{"stored image is linked after the text", "asset://0123abcd.png", true, ask + "\n\n![image](asset://0123abcd.png)"},
		{"missing bytes leave the text unchanged", "", false, ask},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var asked []ClaudeAIFile
			result, err := ParseClaudeAIDetailWithImages(readClaudeAIToolsDetail(t, ""), func(f ClaudeAIFile) (string, bool) {
				asked = append(asked, f)
				return tt.ref, tt.ok
			})
			require.NoError(t, err)
			// The image listed in both files and files_v2 is asked for once;
			// the PDF upload is not an image and keeps its attachment text.
			assert.Equal(t, []ClaudeAIFile{{
				UUID: "33333333-3333-4333-8333-333333333331", Name: "shore.png", Kind: "image",
				PreviewURL: "/api/organizations/11111111-1111-4111-8111-111111111111/files/33333333-3333-4333-8333-333333333331/preview",
			}}, asked)
			require.Len(t, result.Messages, 4)
			assert.Equal(t, tt.wantAsk, result.Messages[0].Content)
			assert.Equal(t, len(tt.wantAsk), result.Messages[0].ContentLength)
			assert.Equal(t, tt.wantAsk, result.Session.FirstMessage)
			plain, err := ParseClaudeAIDetail(readClaudeAIToolsDetail(t, ""))
			require.NoError(t, err)
			assert.Equal(t, plain.Messages[1:], result.Messages[1:])
		})
	}
}

func TestParseClaudeAIDetailWithImages_SkipsMalformedFiles(t *testing.T) {
	input := `{"uuid":"c","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z","current_leaf_message_uuid":"q","chat_messages":[` +
		`{"uuid":"q","parent_message_uuid":"00000000-0000-4000-8000-000000000000","sender":"human","text":"Look",` +
		`"files":["not an object",{"file_kind":"image","file_name":"no-uuid.png"}],` +
		`"files_v2":[{"file_kind":"image","file_uuid":42},{"file_kind":"image","file_uuid":"f1","file_name":"ok.webp"}]}]}`
	var asked []string
	result, err := ParseClaudeAIDetailWithImages([]byte(input), func(f ClaudeAIFile) (string, bool) {
		asked = append(asked, f.UUID)
		return "asset://ok.webp", true
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"f1"}, asked)
	require.Len(t, result.Messages, 1)
	assert.Equal(t, "Look\n\n![image](asset://ok.webp)", result.Messages[0].Content)
}
