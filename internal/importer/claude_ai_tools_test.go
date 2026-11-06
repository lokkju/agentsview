package importer

import (
	"bytes"
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"hash/fnv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

//go:embed testdata/claude_ai_sync/detail_tools.json
var syncToolsDetail string

const toolsChatID = "claude-ai:22222222-2222-4222-8222-222222222222"

// archiveLikeOldParser stores the chat's visible branch the way the parser
// did before it read tool blocks: the same text and ordinals, no tool calls.
func archiveLikeOldParser(t *testing.T, d *db.DB) []db.Message {
	t.Helper()
	result, err := parser.ParseClaudeAIDetail([]byte(syncToolsDetail))
	require.NoError(t, err)
	for i := range result.Messages {
		result.Messages[i].ToolCalls = nil
		result.Messages[i].HasToolUse = false
	}
	_, err = d.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session:           chatGPTSession(result.Session),
		Messages:          claudeAIMessages(toolsChatID, result.Messages),
		SkipSignalUpdates: true,
	}})
	require.NoError(t, err)
	stored, err := d.GetAllMessages(t.Context(), toolsChatID)
	require.NoError(t, err)
	require.Len(t, stored, 4)
	for _, m := range stored {
		require.Empty(t, m.ToolCalls)
	}
	return stored
}

// preToolCallFreshness freezes the full-archive fingerprint format used
// before tool calls were read. It must stay independent of claudeAIFreshness.
func preToolCallFreshness(session *db.Session, leaf string) uint64 {
	hash := fnv.New64a()
	fmt.Fprintf(hash, "2026-03-01T10:05:00.123456Z\x00%s\x00%d", leaf, session.MessageCount)
	for _, field := range []*string{session.TranscriptRevision, session.SessionName, session.EndedAt} {
		fmt.Fprint(hash, "\x00")
		if field != nil {
			fmt.Fprint(hash, *field)
		}
	}
	return hash.Sum64()
}

type storedCall struct{ Name, Input, Result string }

func storedCalls(m db.Message) []storedCall {
	calls := make([]storedCall, 0, len(m.ToolCalls))
	for _, c := range m.ToolCalls {
		calls = append(calls, storedCall{c.ToolName, c.InputJSON, c.ResultContent})
	}
	return calls
}

// pinOldRows pins the reply that gains tool calls and the last reply, with a
// note on the latter.
func pinOldRows(t *testing.T, d *db.DB, old []db.Message) {
	t.Helper()
	note := "keep this"
	_, err := d.PinMessage(t.Context(), toolsChatID, old[1].ID, nil)
	require.NoError(t, err)
	_, err = d.PinMessage(t.Context(), toolsChatID, old[3].ID, &note)
	require.NoError(t, err)
}

// assertRefreshedInPlace checks that the archived chat now carries its tool
// calls, kept its text, ordinals and pins, and left no Trash copy.
func assertRefreshedInPlace(t *testing.T, d *db.DB, old []db.Message) []db.Message {
	t.Helper()
	got, err := d.GetAllMessages(t.Context(), toolsChatID)
	require.NoError(t, err)
	require.Len(t, got, 4)
	for i := range got {
		assert.Equal(t, old[i].Ordinal, got[i].Ordinal)
		assert.Equal(t, old[i].Content, got[i].Content)
	}
	assert.Empty(t, got[0].ToolCalls)
	assert.Empty(t, got[2].ToolCalls)
	assert.True(t, got[1].HasToolUse)
	assert.Equal(t, []storedCall{
		{"web_search", `{"query":"moon phase today"}`, "Lunar calendar\nhttps://example.com/moon\nThe moon is waxing gibbous."},
		{"artifacts", `{"id":"tide-poem","command":"create","type":"text/markdown","title":"Tide Poem","content":"Silver water climbs the sand"}`, "OK"},
	}, storedCalls(got[1]))
	assert.Equal(t, []storedCall{
		{"repl", `{"code":"console.log('Silver water climbs the sand'.split(' ').length)"}`, `{"status":"success","logs":["5"]}`},
	}, storedCalls(got[3]))

	pins, err := d.ListPinnedMessages(t.Context(), toolsChatID, "")
	require.NoError(t, err)
	require.Len(t, pins, 2)
	byOrdinal := map[int]db.PinnedMessage{}
	for _, p := range pins {
		byOrdinal[p.Ordinal] = p
	}
	require.Contains(t, byOrdinal, 1)
	require.Contains(t, byOrdinal, 3)
	assert.Equal(t, got[1].ID, byOrdinal[1].MessageID)
	assert.Nil(t, byOrdinal[1].Note)
	assert.Equal(t, got[3].ID, byOrdinal[3].MessageID)
	require.NotNil(t, byOrdinal[3].Note)
	assert.Equal(t, "keep this", *byOrdinal[3].Note)

	assert.Empty(t, replacedCopies(t, d, toolsChatID))
	return got
}

func TestSyncClaudeAIRefreshesOldArchiveWithToolCallsInPlace(t *testing.T) {
	d := testDB(t)
	old := archiveLikeOldParser(t, d)
	pinOldRows(t, d, old)
	session, err := d.GetSessionFull(t.Context(), toolsChatID)
	require.NoError(t, err)
	require.NotNil(t, session)
	// The chat was cached by a Sync that ran the old parser.
	require.NoError(t, d.UpsertProviderStatHash(t.Context(), parser.AgentClaudeAI, toolsChatID, preToolCallFreshness(session, "answer")))

	details := 0
	fetch := syncOneFetch(t, strings.Replace(syncSummary, "reply", "answer", 1), func() (ClaudeAIResponse, error) {
		details++
		return ClaudeAIResponse{Status: 200, Body: []byte(syncToolsDetail)}, nil
	})
	stats, err := SyncClaudeAI(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, ImportStats{Updated: 1}, stats)
	assert.Equal(t, 1, details)
	assertRefreshedInPlace(t, d, old)

	// Artifact content is searchable through the call's input.
	page, err := d.SearchContent(t.Context(), db.ContentSearchFilter{
		Pattern: "Silver water climbs", Mode: "substring",
		Sources: []string{"tool_input"}, Limit: 50,
	})
	require.NoError(t, err)
	require.NotEmpty(t, page.Matches)
	assert.Equal(t, toolsChatID, page.Matches[0].SessionID)
	assert.Equal(t, 1, page.Matches[0].Ordinal)
	// Web search output is findable from the session view.
	ordinals, err := d.SearchSession(t.Context(), toolsChatID, "Lunar calendar")
	require.NoError(t, err)
	assert.Equal(t, []int{1}, ordinals)

	// The refreshed chat is cached again.
	stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, ImportStats{Skipped: 1}, stats)
	assert.Equal(t, 1, details)
}

// visibleBranchExport turns the detail fixture into a one-conversation export
// holding the visible branch in order, as a Claude.ai data export would.
func visibleBranchExport(t *testing.T) []byte {
	t.Helper()
	var conv map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(syncToolsDetail), &conv))
	var messages []jsontext.Value
	require.NoError(t, json.Unmarshal(conv["chat_messages"], &messages))
	visible, err := json.Marshal(messages[:4])
	require.NoError(t, err)
	conv["chat_messages"] = visible
	out, err := json.Marshal([]map[string]jsontext.Value{conv})
	require.NoError(t, err)
	return out
}

func TestImportClaudeAIRefreshesOldArchiveWithToolCallsInPlace(t *testing.T) {
	d := testDB(t)
	old := archiveLikeOldParser(t, d)
	pinOldRows(t, d, old)

	stats, err := ImportClaudeAI(t.Context(), d, bytes.NewReader(visibleBranchExport(t)), nil)
	require.NoError(t, err)
	assert.Equal(t, ImportStats{Updated: 1}, stats)
	got := assertRefreshedInPlace(t, d, old)
	// Zip import updates the changed rows in place, so they keep their IDs.
	for i := range got {
		assert.Equal(t, old[i].ID, got[i].ID)
	}

	stats, err = ImportClaudeAI(t.Context(), d, bytes.NewReader(visibleBranchExport(t)), nil)
	require.NoError(t, err)
	assert.Equal(t, ImportStats{Skipped: 1}, stats)
}
