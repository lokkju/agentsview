package sync

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

// These agents link a child only from the child's own source, so a usage
// refresh that runs before startup ingestion must find the children itself.
func TestSyncSessionWithSubagentsDiscoversChildLinkedSessionsBeforeStartupSync(t *testing.T) {
	for _, tc := range []struct {
		name   string
		agent  parser.AgentType
		prefix string
		// newSource returns a function that writes one session, linked to
		// parent when parent is not empty.
		newSource func(t *testing.T, root string) func(id, parent string)
	}{
		{name: "muse", agent: parser.AgentMuse, prefix: "muse:", newSource: newMuseFirstUseSource},
		{name: "fx", agent: parser.AgentFx, prefix: "fx:", newSource: newFxFirstUseSource},
		{name: "goose", agent: parser.AgentGoose, prefix: "goose:", newSource: newGooseFirstUseSource},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			database := openTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{
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

// newMuseFirstUseSource nests each child log under its parent's subagent folder.
func newMuseFirstUseSource(t *testing.T, root string) func(id, parent string) {
	t.Helper()
	dirs := make(map[string]string)
	return func(id, parent string) {
		t.Helper()
		dir := filepath.Join(root, "2026", "09", "10", id)
		if parent != "" {
			dir = filepath.Join(dirs[parent], "subagent", id)
		}
		dirs[id] = dir
		require.NoError(t, os.MkdirAll(dir, 0o755))
		record := fmt.Sprintf(`{"schema_version":1,"stream":{"kind":"session","id":%q},"sequence":1,"recorded_at":1789100000000000,"payload_type":"runtime.session","payload":{"kind":"run","event":{"kind":"started","prompt":"%s prompt"}}}`+"\n", id, id)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(record), 0o600))
	}
}

// newFxFirstUseSource writes sibling session folders; a child names its parent
// in subagent/owner.json.
func newFxFirstUseSource(t *testing.T, root string) func(id, parent string) {
	t.Helper()
	return func(id, parent string) {
		t.Helper()
		dir := filepath.Join(root, id)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "session.json"), []byte(`{"schema_version":4,"created_at_ms":1735689601000,"updated_at_ms":1735689602000}`), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(`{"schema_version":3,"seq":1,"timestamp_ms":1735689601000,"event":{"user":{"text":"`+id+` prompt"}}}`+"\n"), 0o600))
		if parent != "" {
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "subagent"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "subagent", "owner.json"), []byte(`{"schema_version":1,"parent_id":"`+parent+`"}`), 0o600))
		}
	}
}

// newGooseFirstUseSource keeps every session in one sessions.db; a child names
// its parent in sessions.parent_session_id.
func newGooseFirstUseSource(t *testing.T, root string) func(id, parent string) {
	t.Helper()
	sessionsDir := filepath.Join(root, "data", "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	source, err := sql.Open("sqlite3", filepath.Join(sessionsDir, parser.GooseDBName))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	_, err = source.ExecContext(t.Context(), gooseSyncTestSchema)
	require.NoError(t, err)
	return func(id, parent string) {
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
	}
}
