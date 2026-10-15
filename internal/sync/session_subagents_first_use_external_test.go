package sync_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// firstUseRecords writes sessions for one agent. id maps a test name to the
// agent's raw session ID; add links a session to parent when parent is not
// empty; remove deletes a session's source.
type firstUseRecords struct {
	root   string
	id     func(name string) string
	add    func(name, parent string)
	remove func(name string)
}

// These agents link a child only from the child's own source, so a usage
// refresh that runs before startup ingestion must find the children itself.
func TestSyncSessionWithSubagentsDiscoversChildLinkedRecordsBeforeStartupSync(t *testing.T) {
	for _, tc := range []struct {
		name       string
		agent      parser.AgentType
		prefix     string
		newRecords func(t *testing.T) firstUseRecords
	}{
		{name: "deepseek harness", agent: parser.AgentDeepSeekHarness, prefix: "deepseek-harness:", newRecords: newDeepSeekHarnessFirstUseRecords},
		{name: "omnigent", agent: parser.AgentOmnigent, prefix: "omnigent:0:", newRecords: newOmnigentFirstUseRecords},
		{name: "antigravity cli", agent: parser.AgentAntigravityCLI, prefix: "antigravity-cli:", newRecords: newAntigravityCLIFirstUseRecords},
		{name: "codex", agent: parser.AgentCodex, prefix: "codex:", newRecords: newCodexFirstUseRecords},
	} {
		for _, childSourceRemoved := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/child_source_removed=%t", tc.name, childSourceRemoved), func(t *testing.T) {
				records := tc.newRecords(t)
				sessionID := func(name string) string { return tc.prefix + records.id(name) }
				database := dbtest.OpenTestDB(t)
				engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
					AgentDirs:                  map[parser.AgentType][]string{tc.agent: {records.root}},
					Machine:                    "local",
					DisableSignalRecomputation: true,
				})
				t.Cleanup(engine.Close)
				records.add("parent", "")
				require.NoError(t, engine.SyncSingleSession(sessionID("parent")))
				records.add("child", "parent")
				if childSourceRemoved {
					require.NoError(t, engine.SyncSingleSession(sessionID("child")))
				}
				records.add("grandchild", "child")
				records.add("unrelated", "")
				if childSourceRemoved {
					records.remove("child")
				}

				require.False(t, engine.StartupReconciled())
				err := engine.SyncSessionWithSubagentsContext(t.Context(), sessionID("parent"))
				if !childSourceRemoved {
					// An archived child whose source is gone may report an
					// error, but the refresh must still reach its descendants.
					require.NoError(t, err)
				}
				assert.False(t, engine.StartupReconciled())
				children, err := database.GetChildSessions(t.Context(), sessionID("parent"))
				require.NoError(t, err)
				require.Len(t, children, 1)
				assert.Equal(t, sessionID("child"), children[0].ID)
				grandchildren, err := database.GetChildSessions(t.Context(), children[0].ID)
				require.NoError(t, err)
				require.Len(t, grandchildren, 1)
				assert.Equal(t, sessionID("grandchild"), grandchildren[0].ID)
				unrelated, err := database.GetSession(t.Context(), sessionID("unrelated"))
				require.NoError(t, err)
				assert.Nil(t, unrelated)
			})
		}
	}
}

// Canonical DeepSeek Harness IDs escape '~' and '%', which the source folder
// and header keep in their raw form.
func TestSyncSessionWithSubagentsFindsEscapedDeepSeekHarnessIDsBeforeStartupSync(t *testing.T) {
	const (
		parentRaw, parentFolder, parentID = "lead~1", "lead~007E1", "deepseek-harness:lead%7E1"
		childRaw, childFolder, childID    = "worker%2", "worker~00252", "deepseek-harness:worker%252"
	)
	root := t.TempDir()
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs:                  map[parser.AgentType][]string{parser.AgentDeepSeekHarness: {root}},
		Machine:                    "local",
		DisableSignalRecomputation: true,
	})
	t.Cleanup(engine.Close)
	harnessSyncWriteLog(t, root, parentFolder, map[string]any{"id": parentRaw}, harnessSyncCompleteTurn("lead answer"))
	harnessSyncWriteLog(t, root, childFolder, map[string]any{
		"id": childRaw, "parentSession": parentRaw, "origin": "subagent", "delegationDepth": 1,
	}, harnessSyncCompleteTurn("worker answer"))

	require.NoError(t, engine.SyncSessionWithSubagentsContext(t.Context(), parentID))
	assert.False(t, engine.StartupReconciled())
	children, err := database.GetChildSessions(t.Context(), parentID)
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, childID, children[0].ID)
}

// newDeepSeekHarnessFirstUseRecords writes sibling session folders; a child
// names its parent in its log header.
func newDeepSeekHarnessFirstUseRecords(t *testing.T) firstUseRecords {
	t.Helper()
	root := t.TempDir()
	paths := make(map[string]string)
	return firstUseRecords{
		root: root,
		id:   func(name string) string { return name },
		add: func(name, parent string) {
			t.Helper()
			var header map[string]any
			if parent != "" {
				header = map[string]any{"parentSession": parent, "origin": "subagent", "delegationDepth": 1}
			}
			paths[name] = harnessSyncWriteLog(t, root, name, header, harnessSyncCompleteTurn(name+" answer"))
		},
		remove: func(name string) {
			t.Helper()
			require.NoError(t, os.RemoveAll(filepath.Dir(paths[name])))
		},
	}
}

// newOmnigentFirstUseRecords keeps every session in one chat.db; a child names
// its parent in conversations.parent_conversation_id.
func newOmnigentFirstUseRecords(t *testing.T) firstUseRecords {
	t.Helper()
	root := t.TempDir()
	source, err := sql.Open("sqlite3", writeOmnigentSplitSyncDB(t, root, 0))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	return firstUseRecords{
		root: root,
		id:   func(name string) string { return name },
		add: func(name, parent string) {
			t.Helper()
			kind, rootID := 1, name
			if parent != "" {
				kind, rootID = 2, "parent"
			}
			_, err := source.ExecContext(t.Context(), `INSERT INTO conversations
				(workspace_id, id, created_at, updated_at, title, parent_conversation_id, root_conversation_id)
				VALUES (0, ?, 1700000000, 1700000001, ?, ?, ?)`, name, name, parent, rootID)
			require.NoError(t, err)
			_, err = source.ExecContext(t.Context(), `INSERT INTO omnigent_conversation_metadata
				(workspace_id, id, kind, workspace) VALUES (0, ?, ?, '/work/project')`, name, kind)
			require.NoError(t, err)
			_, err = source.ExecContext(t.Context(), `INSERT INTO conversation_items
				(workspace_id, conversation_id, id, position, type, data, search_text)
				VALUES (0, ?, ?, 0, 1, ?, 'initial')`, name, name+"_0",
				`{"role":"user","content":[{"type":"input_text","text":"initial"}]}`)
			require.NoError(t, err)
		},
		remove: func(name string) {
			t.Helper()
			for _, table := range []string{"conversation_items WHERE conversation_id = ?", "omnigent_conversation_metadata WHERE id = ?", "conversations WHERE id = ?"} {
				_, err := source.ExecContext(t.Context(), "DELETE FROM "+table, name)
				require.NoError(t, err)
			}
		},
	}
}

// newAntigravityCLIFirstUseRecords writes sibling conversations; a child names
// its parent in its trajectory sidecar's agyReader.parentCascadeId.
func newAntigravityCLIFirstUseRecords(t *testing.T) firstUseRecords {
	t.Helper()
	root := t.TempDir()
	convDir := filepath.Join(root, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))
	ids := map[string]string{
		"parent":     "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		"child":      "11111111-2222-4333-8444-555555555555",
		"grandchild": "66666666-7777-4888-8999-aaaaaaaaaaaa",
		"unrelated":  "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff",
	}
	return firstUseRecords{
		root: root,
		id:   func(name string) string { return ids[name] },
		add: func(name, parent string) {
			t.Helper()
			id := ids[name]
			require.NoError(t, os.WriteFile(filepath.Join(convDir, id+".pb"), []byte("pb-stub"), 0o600))
			trajectory := antigravityCLISingleUserTrajectory(id, name+" prompt")
			if parent != "" {
				trajectory = antigravityCLIParentedTrajectory(id, ids[parent], name+" prompt")
			}
			require.NoError(t, os.WriteFile(filepath.Join(convDir, id+".trajectory.json"), []byte(trajectory), 0o600))
		},
		remove: func(name string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(convDir, ids[name]+".pb")))
			require.NoError(t, os.Remove(filepath.Join(convDir, ids[name]+".trajectory.json")))
		},
	}
}

// newCodexFirstUseRecords writes dated rollouts. Each child is a guardian
// review that names its parent only in its own session_meta, and no parent
// records a spawn. The grandchild starts a day after the others.
func newCodexFirstUseRecords(t *testing.T) firstUseRecords {
	t.Helper()
	root := t.TempDir()
	ids := map[string]string{
		"parent":     "019a0000-0000-7000-8000-000000000001",
		"child":      "019a0000-0000-7000-8000-000000000002",
		"grandchild": "019a0000-0000-7000-8000-000000000003",
		"unrelated":  "019a0000-0000-7000-8000-000000000004",
	}
	paths := make(map[string]string)
	return firstUseRecords{
		root: root,
		id:   func(name string) string { return ids[name] },
		add: func(name, parent string) {
			t.Helper()
			id, day := ids[name], "10"
			if name == "grandchild" {
				day = "11"
			}
			ts := "2026-09-" + day + "T10:00:00Z"
			meta := testjsonl.CodexSessionMetaJSON(id, "/work/project", "codex_cli_rs", ts)
			if parent != "" {
				meta = testjsonl.CodexSubagentSessionMetaVariantJSON(
					"guardian_review", map[string]any{"other": "guardian"},
					id, ids[parent], "/work/project", "codex_cli_rs", ts,
				)
			}
			dir := filepath.Join(root, "2026", "09", day)
			require.NoError(t, os.MkdirAll(dir, 0o755))
			paths[name] = filepath.Join(dir, "rollout-2026-09-"+day+"T10-00-00-"+id+".jsonl")
			content := testjsonl.JoinJSONL(meta, testjsonl.CodexMsgJSON("user", name+" prompt", ts))
			require.NoError(t, os.WriteFile(paths[name], []byte(content), 0o600))
		},
		remove: func(name string) {
			t.Helper()
			require.NoError(t, os.Remove(paths[name]))
		},
	}
}
