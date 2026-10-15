package sync_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// These agents link a child only from the child's own source, so a usage
// refresh that runs before startup ingestion must find the children itself.
func TestSyncSessionWithSubagentsDiscoversChildLinkedRecordsBeforeStartupSync(t *testing.T) {
	for _, tc := range []struct {
		name   string
		agent  parser.AgentType
		prefix string
		// newSource returns a function that writes one session, linked to
		// parent when parent is not empty.
		newSource func(t *testing.T, root string) func(id, parent string)
	}{
		{name: "deepseek harness", agent: parser.AgentDeepSeekHarness, prefix: "deepseek-harness:", newSource: newDeepSeekHarnessFirstUseSource},
		{name: "omnigent", agent: parser.AgentOmnigent, prefix: "omnigent:0:", newSource: newOmnigentFirstUseSource},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			database := dbtest.OpenTestDB(t)
			engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
				AgentDirs:                  map[parser.AgentType][]string{tc.agent: {root}},
				Machine:                    "local",
				DisableSignalRecomputation: true,
			})
			t.Cleanup(engine.Close)
			addSession := tc.newSource(t, root)
			addSession("parent", "")
			require.NoError(t, engine.SyncSingleSession(tc.prefix+"parent"))
			addSession("child", "parent")
			addSession("grandchild", "child")
			addSession("unrelated", "")

			require.False(t, engine.StartupReconciled())
			require.NoError(t, engine.SyncSessionWithSubagentsContext(t.Context(), tc.prefix+"parent"))
			assert.False(t, engine.StartupReconciled())
			children, err := database.GetChildSessions(t.Context(), tc.prefix+"parent")
			require.NoError(t, err)
			require.Len(t, children, 1)
			assert.Equal(t, tc.prefix+"child", children[0].ID)
			grandchildren, err := database.GetChildSessions(t.Context(), children[0].ID)
			require.NoError(t, err)
			require.Len(t, grandchildren, 1)
			assert.Equal(t, tc.prefix+"grandchild", grandchildren[0].ID)
			unrelated, err := database.GetSession(t.Context(), tc.prefix+"unrelated")
			require.NoError(t, err)
			assert.Nil(t, unrelated)
		})
	}
}

// newDeepSeekHarnessFirstUseSource writes sibling session folders; a child
// names its parent in its log header.
func newDeepSeekHarnessFirstUseSource(t *testing.T, root string) func(id, parent string) {
	t.Helper()
	return func(id, parent string) {
		t.Helper()
		var header map[string]any
		if parent != "" {
			header = map[string]any{"parentSession": parent, "origin": "subagent", "delegationDepth": 1}
		}
		harnessSyncWriteLog(t, root, id, header, harnessSyncCompleteTurn(id+" answer"))
	}
}

// newOmnigentFirstUseSource keeps every session in one chat.db; a child names
// its parent in conversations.parent_conversation_id.
func newOmnigentFirstUseSource(t *testing.T, root string) func(id, parent string) {
	t.Helper()
	source, err := sql.Open("sqlite3", writeOmnigentSplitSyncDB(t, root, 0))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	return func(id, parent string) {
		t.Helper()
		kind, rootID := 1, id
		if parent != "" {
			kind, rootID = 2, "parent"
		}
		_, err := source.ExecContext(t.Context(), `INSERT INTO conversations
			(workspace_id, id, created_at, updated_at, title, parent_conversation_id, root_conversation_id)
			VALUES (0, ?, 1700000000, 1700000001, ?, ?, ?)`, id, id, parent, rootID)
		require.NoError(t, err)
		_, err = source.ExecContext(t.Context(), `INSERT INTO omnigent_conversation_metadata
			(workspace_id, id, kind, workspace) VALUES (0, ?, ?, '/work/project')`, id, kind)
		require.NoError(t, err)
		_, err = source.ExecContext(t.Context(), `INSERT INTO conversation_items
			(workspace_id, conversation_id, id, position, type, data, search_text)
			VALUES (0, ?, ?, 0, 1, ?, 'initial')`, id, id+"_0",
			`{"role":"user","content":[{"type":"input_text","text":"initial"}]}`)
		require.NoError(t, err)
	}
}
