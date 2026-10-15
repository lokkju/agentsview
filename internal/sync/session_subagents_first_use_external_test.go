package sync_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
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
		{name: "traex", agent: parser.AgentTraeX, prefix: "traex:", newRecords: newCodexFirstUseRecords},
		{name: "augure code", agent: parser.AgentAugureCode, prefix: "augure-code:", newRecords: newCodexFirstUseRecords},
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

// Codex child discovery must survive revert pages that share a child's thread
// ID, parents that were archived, and intermediates whose source is gone.
func TestSyncSessionWithSubagentsCodexChildEdgesBeforeStartupSync(t *testing.T) {
	const (
		parent     = "019a0000-0000-7000-8000-0000000000a1"
		child      = "019a0000-0000-7000-8000-0000000000a2"
		grandchild = "019a0000-0000-7000-8000-0000000000a3"
		rollout    = "019a0000-0000-7000-8000-0000000000b1"
	)
	type codexRollout struct{ dir, name, content string }
	write := func(t *testing.T, r codexRollout) string {
		t.Helper()
		require.NoError(t, os.MkdirAll(r.dir, 0o755))
		path := filepath.Join(r.dir, r.name)
		require.NoError(t, os.WriteFile(path, []byte(r.content), 0o600))
		return path
	}
	rolloutName := func(day, clock, key string) string {
		return "rollout-2026-09-" + day + "T" + clock + "-" + key + ".jsonl"
	}
	meta := func(id, parentID, ts string) string {
		if parentID == "" {
			return testjsonl.CodexSessionMetaJSON(id, "/work/project", "codex_cli_rs", ts)
		}
		return testjsonl.CodexSubagentSessionMetaVariantJSON(
			"guardian_review", map[string]any{"other": "guardian"},
			id, parentID, "/work/project", "codex_cli_rs", ts,
		)
	}
	transcript := func(metaLine string, timestamps ...string) string {
		lines := []string{metaLine}
		for _, ts := range timestamps {
			lines = append(lines, testjsonl.CodexMsgJSON("user", "prompt at "+ts, ts))
		}
		return testjsonl.JoinJSONL(lines...)
	}
	newEngine := func(t *testing.T, roots ...string) (*sync.Engine, *db.DB) {
		t.Helper()
		database := dbtest.OpenTestDB(t)
		engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
			AgentDirs:                  map[parser.AgentType][]string{parser.AgentCodex: roots},
			Machine:                    "local",
			DisableSignalRecomputation: true,
		})
		t.Cleanup(engine.Close)
		return engine, database
	}
	childIDs := func(t *testing.T, database *db.DB, id string) []string {
		t.Helper()
		children, err := database.GetChildSessions(t.Context(), id)
		require.NoError(t, err)
		ids := make([]string, 0, len(children))
		for _, c := range children {
			ids = append(ids, c.ID)
		}
		return ids
	}

	t.Run("revert page in the child's day folder", func(t *testing.T) {
		root := t.TempDir()
		day := filepath.Join(root, "2026", "09", "10")
		write(t, codexRollout{day, rolloutName("10", "10-00-00", parent), transcript(meta(parent, "", "2026-09-10T10:00:00Z"), "2026-09-10T10:00:00Z")})
		write(t, codexRollout{day, rolloutName("10", "10-05-00", child), transcript(meta(child, parent, "2026-09-10T10:05:00Z"), "2026-09-10T10:05:00Z")})
		page := testjsonl.CodexSessionMetaWithFieldsJSON(child, "/work/project", "codex_cli_rs", "2026-09-10T11:00:00Z", map[string]any{
			"history_mode": "paginated",
			"history_base": map[string]any{"thread_id": child},
		})
		write(t, codexRollout{day, rolloutName("10", "11-00-00", child+"_"+rollout), transcript(page, "2026-09-10T11:00:00Z")})
		engine, database := newEngine(t, root)

		require.NoError(t, engine.SyncSessionWithSubagentsContext(t.Context(), "codex:"+parent))
		assert.Equal(t, []string{"codex:" + child}, childIDs(t, database, "codex:"+parent))
		assert.Equal(t, []string{"codex:" + child + "_" + rollout}, childIDs(t, database, "codex:"+child))
	})

	t.Run("archived parent searches the dated sessions folder", func(t *testing.T) {
		home := t.TempDir()
		sessions, archived := filepath.Join(home, "sessions"), filepath.Join(home, "archived_sessions")
		write(t, codexRollout{archived, rolloutName("10", "10-00-00", parent), transcript(meta(parent, "", "2026-09-10T10:00:00Z"), "2026-09-10T10:00:00Z")})
		write(t, codexRollout{filepath.Join(sessions, "2026", "09", "10"), rolloutName("10", "10-05-00", child), transcript(meta(child, parent, "2026-09-10T10:05:00Z"), "2026-09-10T10:05:00Z")})
		engine, database := newEngine(t, sessions, archived)

		require.NoError(t, engine.SyncSessionWithSubagentsContext(t.Context(), "codex:"+parent))
		assert.Equal(t, []string{"codex:" + child}, childIDs(t, database, "codex:"+parent))
	})

	t.Run("missing intermediate keeps its archived last write", func(t *testing.T) {
		root := t.TempDir()
		write(t, codexRollout{filepath.Join(root, "2026", "09", "10"), rolloutName("10", "10-00-00", parent), transcript(meta(parent, "", "2026-09-10T10:00:00Z"), "2026-09-10T10:00:00Z")})
		childTranscript := transcript(meta(child, parent, "2026-09-10T10:05:00Z"), "2026-09-10T10:05:00Z", "2026-09-14T09:00:00Z")
		childPath := write(t, codexRollout{filepath.Join(root, "2026", "09", "10"), rolloutName("10", "10-05-00", child), childTranscript})
		engine, database := newEngine(t, root)
		require.NoError(t, engine.SyncSingleSession("codex:"+parent))
		require.NoError(t, engine.SyncSingleSession("codex:"+child))
		write(t, codexRollout{filepath.Join(root, "2026", "09", "14"), rolloutName("14", "09-00-00", grandchild), transcript(meta(grandchild, child, "2026-09-14T09:00:00Z"), "2026-09-14T09:00:00Z")})
		require.NoError(t, os.Remove(childPath))

		// The removed child's own refresh may report its missing source.
		_ = engine.SyncSessionWithSubagentsContext(t.Context(), "codex:"+parent)
		assert.Equal(t, []string{"codex:" + grandchild}, childIDs(t, database, "codex:"+child))
	})
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
