package sync

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

// firstUseSource writes sessions for one agent. add links a session to parent
// when parent is not empty; remove deletes a session's source. A nil remove
// means the agent keeps child links only in the parent's source.
type firstUseSource struct {
	root   string
	add    func(id, parent string)
	remove func(id string)
}

// These agents link a child from the child's own source, or from a parent
// record that tool calls do not name, so a usage refresh that runs before
// startup ingestion must find the children itself.
func TestSyncSessionWithSubagentsDiscoversChildLinkedSessionsBeforeStartupSync(t *testing.T) {
	for _, tc := range []struct {
		name      string
		agent     parser.AgentType
		prefix    string
		newSource func(t *testing.T) firstUseSource
	}{
		{name: "muse", agent: parser.AgentMuse, prefix: "muse:", newSource: newMuseFirstUseSource},
		{name: "fx", agent: parser.AgentFx, prefix: "fx:", newSource: newFxFirstUseSource},
		{name: "goose", agent: parser.AgentGoose, prefix: "goose:", newSource: newGooseFirstUseSource},
		{name: "crush", agent: parser.AgentCrush, prefix: "crush:", newSource: newCrushFirstUseSource},
		{name: "evener", agent: parser.AgentEvener, prefix: "evener:", newSource: newEvenerFirstUseSource},
		{name: "roo code", agent: parser.AgentRooCode, prefix: "roocode:", newSource: newRooCodeFirstUseSource},
		{name: "grok", agent: parser.AgentGrok, prefix: "grok:", newSource: newGrokFirstUseSource},
	} {
		for _, childSourceRemoved := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/child_source_removed=%t", tc.name, childSourceRemoved), func(t *testing.T) {
				source := tc.newSource(t)
				if childSourceRemoved && source.remove == nil {
					t.Skip("only the parent's source links its children")
				}
				database := openTestDB(t)
				engine := NewEngine(t.Context(), database, EngineConfig{
					AgentDirs:                  map[parser.AgentType][]string{tc.agent: {source.root}},
					Machine:                    "local",
					DisableSignalRecomputation: true,
				})
				t.Cleanup(engine.Close)
				source.add("parent", "")
				require.NoError(t, engine.SyncSingleSession(tc.prefix+"parent"))
				source.add("child", "parent")
				if childSourceRemoved {
					require.NoError(t, engine.SyncSingleSession(tc.prefix+"child"))
				}
				source.add("grandchild", "child")
				source.add("unrelated", "")
				if childSourceRemoved {
					source.remove("child")
				}

				require.False(t, engine.StartupReconciled())
				err := engine.SyncSessionWithSubagentsContext(t.Context(), tc.prefix+"parent")
				if !childSourceRemoved {
					// An archived child whose source is gone may report an
					// error, but the refresh must still reach its descendants.
					require.NoError(t, err)
				}
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
}

// newMuseFirstUseSource nests each child log under its parent's subagent folder.
func newMuseFirstUseSource(t *testing.T) firstUseSource {
	t.Helper()
	root := t.TempDir()
	dirs := make(map[string]string)
	return firstUseSource{
		root: root,
		add: func(id, parent string) {
			t.Helper()
			dir := filepath.Join(root, "2026", "09", "10", id)
			if parent != "" {
				dir = filepath.Join(dirs[parent], "subagent", id)
			}
			dirs[id] = dir
			require.NoError(t, os.MkdirAll(dir, 0o755))
			record := fmt.Sprintf(`{"schema_version":1,"stream":{"kind":"session","id":%q},"sequence":1,"recorded_at":1789100000000000,"payload_type":"runtime.session","payload":{"kind":"run","event":{"kind":"started","prompt":"%s prompt"}}}`+"\n", id, id)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(record), 0o600))
		},
		remove: func(id string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(dirs[id], "session.jsonl")))
		},
	}
}

// newFxFirstUseSource writes sibling session folders; a child names its parent
// in subagent/owner.json.
func newFxFirstUseSource(t *testing.T) firstUseSource {
	t.Helper()
	root := t.TempDir()
	return firstUseSource{
		root: root,
		add: func(id, parent string) {
			t.Helper()
			dir := filepath.Join(root, id)
			require.NoError(t, os.MkdirAll(dir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "session.json"), []byte(`{"schema_version":4,"created_at_ms":1735689601000,"updated_at_ms":1735689602000}`), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(`{"schema_version":3,"seq":1,"timestamp_ms":1735689601000,"event":{"user":{"text":"`+id+` prompt"}}}`+"\n"), 0o600))
			if parent != "" {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "subagent"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "subagent", "owner.json"), []byte(`{"schema_version":1,"parent_id":"`+parent+`"}`), 0o600))
			}
		},
		remove: func(id string) {
			t.Helper()
			require.NoError(t, os.RemoveAll(filepath.Join(root, id)))
		},
	}
}

// newGooseFirstUseSource keeps every session in one sessions.db; a child names
// its parent in sessions.parent_session_id.
func newGooseFirstUseSource(t *testing.T) firstUseSource {
	t.Helper()
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "data", "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	source, err := sql.Open("sqlite3", filepath.Join(sessionsDir, parser.GooseDBName))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	_, err = source.ExecContext(t.Context(), gooseSyncTestSchema)
	require.NoError(t, err)
	return firstUseSource{
		root: root,
		add: func(id, parent string) {
			t.Helper()
			_, err := source.ExecContext(t.Context(), `
				INSERT INTO sessions (id, name, working_dir, created_at, updated_at, parent_session_id)
				VALUES (?, ?, '/work/acme-app', '2023-11-14 22:13:00', '2023-11-14 22:13:25', ?)
			`, id, id, parent)
			require.NoError(t, err)
			_, err = source.ExecContext(t.Context(), `
				INSERT INTO messages (message_id, session_id, role, content_json, created_timestamp, timestamp, metadata_json)
				VALUES (?, ?, 'user', ?, 1700000000, '2023-11-14 22:13:20', '{}')
			`, id+"-msg", id, `[{"type":"text","text":"`+id+` prompt"}]`)
			require.NoError(t, err)
		},
		remove: func(id string) {
			t.Helper()
			_, err := source.ExecContext(t.Context(), `DELETE FROM messages WHERE session_id = ?`, id)
			require.NoError(t, err)
			_, err = source.ExecContext(t.Context(), `DELETE FROM sessions WHERE id = ?`, id)
			require.NoError(t, err)
		},
	}
}

// newCrushFirstUseSource keeps every session in one crush.db. Child IDs omit
// the "$$<tool-call-id>" suffix, so only sessions.parent_session_id links them.
func newCrushFirstUseSource(t *testing.T) firstUseSource {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), ".crush")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	source, err := sql.Open("sqlite3", filepath.Join(dataDir, parser.CrushDBName))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	_, err = source.ExecContext(t.Context(), crushSyncTestSchema)
	require.NoError(t, err)
	return firstUseSource{
		root: dataDir,
		add: func(id, parent string) {
			t.Helper()
			_, err := source.ExecContext(t.Context(), `
				INSERT INTO sessions (id, parent_session_id, title, message_count, created_at, updated_at)
				VALUES (?, NULLIF(?, ''), ?, 1, 1789093626, 1789093746)
			`, id, parent, id)
			require.NoError(t, err)
			_, err = source.ExecContext(t.Context(), `
				INSERT INTO messages (id, session_id, role, parts, created_at, updated_at)
				VALUES (?, ?, 'user', ?, 1789093626, 1789093626)
			`, id+"-msg", id, `[{"type":"text","data":{"text":"`+id+` prompt"}}]`)
			require.NoError(t, err)
		},
		remove: func(id string) {
			t.Helper()
			_, err := source.ExecContext(t.Context(), `DELETE FROM messages WHERE session_id = ?`, id)
			require.NoError(t, err)
			_, err = source.ExecContext(t.Context(), `DELETE FROM sessions WHERE id = ?`, id)
			require.NoError(t, err)
		},
	}
}

// newEvenerFirstUseSource writes transcripts whose header and metadata name
// the parent. No parent records a delegate result, as when a child is still
// running.
func newEvenerFirstUseSource(t *testing.T) firstUseSource {
	t.Helper()
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessions, 0o755))
	demo, err := os.ReadFile("testdata/evener/demo.transcript.jsonl")
	require.NoError(t, err)
	return firstUseSource{
		root: root,
		add: func(id, parent string) {
			t.Helper()
			header := `"session_id":"` + id + `"`
			meta := map[string]any{"id": id}
			if parent != "" {
				header += `,"parent_session_id":"` + parent + `"`
				meta["parent_session_id"] = parent
				meta["is_subagent"] = true
			}
			transcript := strings.Replace(string(demo), `"session_id":"demo"`, header, 1)
			require.NoError(t, os.WriteFile(filepath.Join(sessions, id+".transcript.jsonl"), []byte(transcript), 0o600))
			data, err := json.Marshal(meta)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(sessions, id+".meta.json"), data, 0o600))
		},
		remove: func(id string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(sessions, id+".transcript.jsonl")))
			require.NoError(t, os.Remove(filepath.Join(sessions, id+".meta.json")))
		},
	}
}

// newRooCodeFirstUseSource lists each child in its parent's childIds, and each
// child names its parent in parentTaskId. With no ui_messages.json there is no
// newTask call to name a child.
func newRooCodeFirstUseSource(t *testing.T) firstUseSource {
	t.Helper()
	root := t.TempDir()
	items := make(map[string]map[string]any)
	write := func(id string) {
		t.Helper()
		dir := filepath.Join(root, "tasks", id)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		data, err := json.Marshal(items[id])
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "history_item.json"), data, 0o600))
	}
	return firstUseSource{
		root: root,
		add: func(id, parent string) {
			t.Helper()
			items[id] = map[string]any{
				"id": id, "number": 1, "ts": 1688836851000, "task": id + " task",
				"tokensIn": 10, "tokensOut": 20, "workspace": "/work/project", "status": "completed",
			}
			if parent != "" {
				items[id]["parentTaskId"] = parent
				childIDs, _ := items[parent]["childIds"].([]string)
				items[parent]["childIds"] = append(childIDs, id)
				write(parent)
			}
			write(id)
		},
		remove: func(id string) {
			t.Helper()
			require.NoError(t, os.RemoveAll(filepath.Join(root, "tasks", id)))
		},
	}
}

// newGrokFirstUseSource records each child in its parent's
// subagents/<child>/meta.json. No parent chat history names a child, as for a
// resumed or worktree spawn, and each child lives in its own worktree cwd
// group.
func newGrokFirstUseSource(t *testing.T) firstUseSource {
	t.Helper()
	root := t.TempDir()
	dirs := make(map[string]string)
	writeJSON := func(path string, value any) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		data, err := json.Marshal(value)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0o600))
	}
	return firstUseSource{
		root: root,
		add: func(id, parent string) {
			t.Helper()
			cwd := "/work/project"
			summary := map[string]any{
				"session_summary": id + " task", "created_at": "2026-07-18T10:00:00Z", "updated_at": "2026-07-18T10:05:00Z",
				"num_messages": 1, "num_chat_messages": 1, "current_model_id": "grok-4.5", "agent_name": "grok-build",
			}
			if parent != "" {
				cwd = "/work/worktree-" + id
				summary["session_kind"] = "subagent"
				writeJSON(filepath.Join(dirs[parent], "subagents", id, "meta.json"), map[string]any{
					"subagent_id": id, "parent_session_id": parent, "child_session_id": id, "status": "completed",
				})
			}
			summary["info"] = map[string]any{"id": id, "cwd": cwd}
			dir := filepath.Join(root, strings.ReplaceAll(cwd, "/", "%2F"), id)
			dirs[id] = dir
			writeJSON(filepath.Join(dir, "summary.json"), summary)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "chat_history.jsonl"), []byte(`{"type":"user","content":"`+id+` prompt"}`+"\n"), 0o600))
		},
	}
}
