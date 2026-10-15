package parser

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// SubagentTranscriptRoot returns the source scope holding a session's children.
func SubagentTranscriptRoot(agent AgentType, sessionPath string) string {
	if sessionPath == "" {
		return ""
	}
	switch agent {
	case AgentCodex, AgentTraeX, AgentAugureCode:
		return ""
	case AgentShelley:
		if _, _, ok := parseShelleyVirtualPath(sessionPath); ok {
			return sessionPath
		}
		return ""
	case AgentOpenCode, AgentKilo, AgentMiMoCode, AgentIcodemate:
		if _, ok := openCodeFamilySubagentRoot(agent, sessionPath); ok {
			return sessionPath
		}
		return ClaudeSubagentTranscriptRoot(sessionPath)
	case AgentPositAssistant:
		return filepath.Join(filepath.Dir(sessionPath), positAssistantSubagentsDir)
	case AgentMuse:
		if filepath.Base(sessionPath) == museLogName {
			return filepath.Join(filepath.Dir(sessionPath), museSubagentDirName)
		}
		return ""
	case AgentFx:
		// fx children are sibling session folders under the same root.
		if dir := filepath.Dir(sessionPath); filepath.Base(sessionPath) == "events.jsonl" && fxSessionIDValid(filepath.Base(dir)) {
			return filepath.Dir(dir)
		}
		return ""
	case AgentDeepSeekHarness:
		// Logs live at <root>/<project>/<id>/session*.jsonl[.zstd]; a child's
		// project follows its own cwd, so search the whole root.
		root := filepath.Dir(filepath.Dir(filepath.Dir(sessionPath)))
		if _, _, _, ok := deepSeekHarnessPathParts(root, sessionPath); ok {
			return root
		}
		return ""
	case AgentGoose:
		if _, _, ok := ParseVirtualSourcePathForBase(sessionPath, GooseDBName); ok {
			return sessionPath
		}
		return ""
	case AgentOmnigent:
		if _, _, ok := parseOmnigentVirtualPath(sessionPath); ok {
			return sessionPath
		}
		return ""
	case AgentWorkBuddy:
		if stem, ok := strings.CutSuffix(sessionPath, ".jsonl"); ok {
			return filepath.Join(stem, "subagents")
		}
		return ""
	case AgentPi, AgentOMP, AgentOMO, AgentStepCode:
		if root, ok := strings.CutSuffix(sessionPath, ".jsonl"); ok {
			return root
		}
		return ""
	case AgentCursor:
		if isS3URI(sessionPath) {
			return CursorS3SubagentRoot(CursorS3SourceRoot(sessionPath), sessionPath)
		}
		loc, ok := cursorTranscriptLocationFromPath(sessionPath)
		if !ok {
			return ""
		}
		dir := filepath.Dir(sessionPath)
		if loc.ParentRawID != "" {
			dir = filepath.Dir(filepath.Dir(dir))
		} else if filepath.Base(dir) == loc.RawID {
			dir = filepath.Dir(dir)
		}
		return filepath.Join(dir, loc.RawID, cursorSubagentsDirName)
	default:
		return ClaudeSubagentTranscriptRoot(sessionPath)
	}
}

// SubagentTranscriptDiscovery shares source inventories within one refresh.
type SubagentTranscriptDiscovery struct {
	inventories map[parentLinkScope]parentLinkInventory
}

// parentLinkScope names one source folder whose sessions link to their
// parents only from the child side.
type parentLinkScope struct {
	agent AgentType
	root  string
}

// descendantPaths lists one scope's sessions once per refresh, then returns
// the sources that descend from parentID.
func (d *SubagentTranscriptDiscovery) descendantPaths(
	agent AgentType, root, parentID string,
	discover func() (parentLinkInventory, error),
) ([]string, error) {
	scope := parentLinkScope{agent: agent, root: root}
	inventory, listed := d.inventories[scope]
	if !listed {
		var err error
		inventory, err = discover()
		if err != nil {
			return nil, err
		}
		if d.inventories == nil {
			d.inventories = make(map[parentLinkScope]parentLinkInventory)
		}
		d.inventories[scope] = inventory
	}
	return inventory.descendantPaths(parentID), nil
}

// Paths discovers children within the requested session's source tree.
// rawSessionID keeps retained storage parents reachable after their source disappears.
func (d *SubagentTranscriptDiscovery) Paths(ctx context.Context, agent AgentType, sessionPath, rawSessionID string) ([]string, error) {
	root := SubagentTranscriptRoot(agent, sessionPath)
	if root == "" {
		return nil, nil
	}
	switch agent {
	case AgentWorkBuddy:
		return workBuddySubagentTranscriptPaths(ctx, root)
	case AgentShelley:
		dbPath, parentID, _ := parseShelleyVirtualPath(sessionPath)
		conn, err := OpenShelleyDB(dbPath)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		rows, err := conn.QueryContext(ctx,
			`WITH RECURSIVE parents AS MATERIALIZED (
				SELECT conversation_id AS id, TRIM(parent_conversation_id) AS parent FROM conversations
			), descendants(id) AS (
				SELECT id FROM parents WHERE parent = ?
				UNION
				SELECT c.id FROM parents c JOIN descendants d ON c.parent = d.id
			)
			SELECT id FROM descendants ORDER BY id`,
			parentID,
		)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var paths []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			if IsValidSessionID(id) {
				paths = append(paths, ShelleyVirtualPath(dbPath, id))
			}
		}
		return paths, rows.Err()
	case AgentOpenCode, AgentKilo, AgentMiMoCode, AgentIcodemate:
		if dbPath, parentID, ok := parseOpenCodeFormatVirtualPath(openCodeFormatForAgent(agent).dbName, sessionPath); ok {
			return openCodeDescendantPaths(ctx, dbPath, parentID)
		}
		if projectDir, ok := openCodeFamilySubagentRoot(agent, sessionPath); ok {
			return d.descendantPaths(agent, projectDir, rawSessionID, func() (parentLinkInventory, error) {
				return discoverOpenCodeStorageSubagents(projectDir)
			})
		}
		return ClaudeSubagentTranscriptPaths(sessionPath), nil
	case AgentPositAssistant:
		var sources []SourceRef
		positAssistantSourceSet{}.collectConversationSources("", filepath.Dir(sessionPath), "", "", &sources, make(map[string]struct{}))
		paths := make([]string, 0, len(sources))
		for _, source := range sources {
			if source.DisplayPath != sessionPath {
				paths = append(paths, source.DisplayPath)
			}
		}
		slices.Sort(paths)
		return paths, nil
	case AgentMuse:
		var paths []string
		err := collectMuseDescendantLogs(root, &paths)
		slices.Sort(paths)
		return paths, err
	case AgentFx:
		return d.descendantPaths(agent, root, rawSessionID, func() (parentLinkInventory, error) {
			return discoverFxSubagents(ctx, root)
		})
	case AgentDeepSeekHarness:
		return d.descendantPaths(agent, root, decodeDeepSeekHarnessCanonicalRawID(rawSessionID), func() (parentLinkInventory, error) {
			return discoverDeepSeekHarnessSubagents(ctx, root)
		})
	case AgentGoose:
		dbPath, parentID, _ := ParseVirtualSourcePathForBase(sessionPath, GooseDBName)
		return gooseDescendantPaths(ctx, dbPath, parentID)
	case AgentOmnigent:
		return omnigentDescendantPaths(ctx, sessionPath)
	case AgentPi, AgentOMP, AgentOMO, AgentStepCode:
		sources, err := NewJSONLSourceSet(agent, []string{root}, WithRecursive(), WithSymlinkFollowing(), WithIncludePath(isPiSourcePath)).Discover(ctx)
		paths := make([]string, 0, len(sources))
		for _, source := range sources {
			paths = append(paths, source.DisplayPath)
		}
		return paths, err
	case AgentCursor:
		seen := make(map[string]string)
		cursorCollectSubagentTranscripts(seen, root)
		paths := make([]string, 0, len(seen))
		for _, path := range seen {
			paths = append(paths, path)
		}
		slices.Sort(paths)
		return paths, nil
	default:
		return ClaudeSubagentTranscriptPaths(sessionPath), nil
	}
}

// SubagentPathsIncludeDescendants reports whether SubagentTranscriptDiscovery.Paths
// returns every descendant of sessionPath, not only direct children, so
// callers need not search the returned children again.
func SubagentPathsIncludeDescendants(agent AgentType, sessionPath string) bool {
	switch agent {
	case AgentShelley, AgentMuse, AgentFx, AgentDeepSeekHarness, AgentGoose, AgentOmnigent:
		return true
	case AgentOpenCode, AgentKilo, AgentMiMoCode, AgentIcodemate:
		_, ok := openCodeFamilySubagentRoot(agent, sessionPath)
		return ok
	default:
		return false
	}
}

// openCodeFamilySubagentRoot returns the database or storage project folder
// that links a session to its descendants through their parent IDs. Other
// paths, such as Icodemate JSONL transcripts, use Claude-style subagent
// folders instead.
func openCodeFamilySubagentRoot(agent AgentType, sessionPath string) (string, bool) {
	format := openCodeFormatForAgent(agent)
	if _, _, ok := parseOpenCodeFormatVirtualPath(format.dbName, sessionPath); ok {
		return sessionPath, true
	}
	// Storage sessions live at storage/<sessionSubdir>/<project>/<id>.json.
	projectDir := filepath.Dir(sessionPath)
	if filepath.Ext(sessionPath) == ".json" && filepath.Base(filepath.Dir(projectDir)) == format.sessionSubdir {
		return projectDir, true
	}
	return "", false
}

func openCodeFormatForAgent(agent AgentType) openCodeFormat {
	for _, format := range []openCodeFormat{openCodeFmt, kiloFmt, mimoFmt, icodemateFmt} {
		if format.agent == agent {
			return format
		}
	}
	return openCodeFormat{}
}

// openCodeDescendantPaths finds sessions linked to parentID only through their
// own parent_id, which parent parsing never sees.
func openCodeDescendantPaths(ctx context.Context, dbPath, parentID string) ([]string, error) {
	conn, err := openOpenCodeDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	tables, idle, migration, err := openCodeSessionTablesCached(ctx, conn, dbPath)
	if err != nil {
		return nil, err
	}
	sources := make([]string, 0, len(tables))
	for _, table := range tables {
		sources = append(sources, "SELECT id, TRIM(parent_id) AS parent FROM "+openCodeSessionFrom(table, idle, migration))
	}
	rows, err := conn.QueryContext(ctx,
		`WITH RECURSIVE parents AS MATERIALIZED (`+strings.Join(sources, " UNION ")+`),
		descendants(id) AS (
			SELECT id FROM parents WHERE parent = ?
			UNION
			SELECT c.id FROM parents c JOIN descendants d ON c.parent = d.id
		)
		SELECT id FROM descendants ORDER BY id`,
		parentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if IsValidSessionID(id) {
			paths = append(paths, OpenCodeSQLiteVirtualPath(dbPath, id))
		}
	}
	return paths, rows.Err()
}

// parentLinkInventory maps each parent ID to its children and each session ID
// to its source path.
type parentLinkInventory struct {
	children map[string][]string
	paths    map[string]string
}

func discoverOpenCodeStorageSubagents(projectDir string) (parentLinkInventory, error) {
	inventory := parentLinkInventory{
		children: make(map[string][]string),
		paths:    make(map[string]string),
	}
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return inventory, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(projectDir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return inventory, err
		}
		var session openCodeStorageSessionFile
		if err := json.Unmarshal(raw, &session); err != nil || session.ID == "" {
			continue
		}
		inventory.paths[session.ID] = path
		if session.ParentID != "" {
			inventory.children[session.ParentID] = append(inventory.children[session.ParentID], session.ID)
		}
	}
	return inventory, nil
}

func (inventory parentLinkInventory) descendantPaths(parentID string) []string {
	var descendants []string
	seen := map[string]bool{parentID: true}
	queue := inventory.children[parentID]
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		descendants = append(descendants, inventory.paths[id])
		queue = append(queue, inventory.children[id]...)
	}
	slices.Sort(descendants)
	return descendants
}

// collectMuseDescendantLogs walks nested subagent folders, where each child
// keeps its own children under <child>/subagent.
func collectMuseDescendantLogs(subagentDir string, paths *[]string) error {
	entries, err := os.ReadDir(subagentDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !IsValidSessionID(entry.Name()) {
			continue
		}
		dir := filepath.Join(subagentDir, entry.Name())
		if log := filepath.Join(dir, museLogName); IsRegularFile(log) {
			*paths = append(*paths, log)
		}
		if err := collectMuseDescendantLogs(filepath.Join(dir, museSubagentDirName), paths); err != nil {
			return err
		}
	}
	return nil
}

// discoverFxSubagents reads each session folder's subagent/owner.json, the
// only record of an fx parent link.
func discoverFxSubagents(ctx context.Context, root string) (parentLinkInventory, error) {
	inventory := parentLinkInventory{
		children: make(map[string][]string),
		paths:    make(map[string]string),
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return inventory, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return inventory, err
		}
		id := entry.Name()
		if !entry.IsDir() || !fxSessionIDValid(id) {
			continue
		}
		owner, err := readFxMetadata[fxOwner](filepath.Join(root, id, "subagent", "owner.json"))
		if err != nil || owner == nil || !fxSessionIDValid(owner.Parent) {
			continue
		}
		events := filepath.Join(root, id, "events.jsonl")
		if !IsRegularFile(events) {
			continue
		}
		inventory.paths[id] = events
		inventory.children[owner.Parent] = append(inventory.children[owner.Parent], id)
	}
	return inventory, nil
}

// discoverDeepSeekHarnessSubagents reads the header of each preferred log,
// the only record of a DeepSeek Harness parent link.
func discoverDeepSeekHarnessSubagents(ctx context.Context, root string) (parentLinkInventory, error) {
	inventory := parentLinkInventory{
		children: make(map[string][]string),
		paths:    make(map[string]string),
	}
	sources, err := newDeepSeekHarnessSourceSet([]string{root}).Discover(ctx)
	if err != nil {
		return inventory, err
	}
	for _, source := range sources {
		header, err := readDeepSeekHarnessHeader(ctx, source.DisplayPath)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return inventory, err
		}
		if err != nil || header.ParentSession == "" {
			continue
		}
		inventory.paths[header.ID] = source.DisplayPath
		inventory.children[header.ParentSession] = append(inventory.children[header.ParentSession], header.ID)
	}
	return inventory, nil
}

// gooseDescendantPaths follows sessions.parent_session_id, which parent
// parsing never reads.
func gooseDescendantPaths(ctx context.Context, dbPath, parentID string) ([]string, error) {
	conn, err := openGooseDB(dbPath, false)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	columns, err := gooseTableColumns(ctx, conn, "sessions")
	if err != nil || !columns["parent_session_id"] {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx,
		`WITH RECURSIVE parents AS MATERIALIZED (
			SELECT CAST(id AS TEXT) AS id, TRIM(parent_session_id) AS parent FROM sessions
		), descendants(id) AS (
			SELECT id FROM parents WHERE parent = ?
			UNION
			SELECT c.id FROM parents c JOIN descendants d ON c.parent = d.id
		)
		SELECT id FROM descendants ORDER BY id`,
		parentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if IsValidSessionID(id) {
			paths = append(paths, GooseSQLiteVirtualPath(dbPath, id))
		}
	}
	return paths, rows.Err()
}

// omnigentDescendantPaths follows conversations.parent_conversation_id within
// the parent's workspace. root_conversation_id names only the top of a tree,
// so it cannot find the descendants of an intermediate conversation.
func omnigentDescendantPaths(ctx context.Context, sessionPath string) ([]string, error) {
	dbPath, key, _ := parseOmnigentVirtualPath(sessionPath)
	member, err := omnigentMemberForSchema(key)
	if err != nil {
		return nil, err
	}
	conn, err := openOmnigentDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	schema, err := detectOmnigentSchema(ctx, conn)
	if err != nil {
		return nil, err
	}
	parent := omnigentIDArg(schema, member.rawID)
	rows, err := conn.QueryContext(ctx,
		`WITH RECURSIVE descendants(id) AS (
			SELECT id FROM conversations
			 WHERE workspace_id = ? AND parent_conversation_id = ? AND id != ?
			UNION
			SELECT c.id FROM conversations c JOIN descendants d ON c.parent_conversation_id = d.id
			 WHERE c.workspace_id = ?
		)
		SELECT `+omnigentIDExpr(schema, "id")+` FROM descendants WHERE id != ? ORDER BY 1`,
		member.workspaceID, parent, parent, member.workspaceID, parent,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if IsValidSessionID(id) {
			child := omnigentMemberID{workspaceID: member.workspaceID, rawID: id}
			paths = append(paths, VirtualSourcePath(dbPath, child.key()))
		}
	}
	return paths, rows.Err()
}
