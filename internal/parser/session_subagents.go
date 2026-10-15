package parser

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// SubagentTranscriptRoot returns the source scope holding a session's children.
func SubagentTranscriptRoot(agent AgentType, sessionPath string) string {
	if sessionPath == "" {
		return ""
	}
	switch agent {
	case AgentCodex:
		if _, ok := codexDatedDay(sessionPath); ok {
			return sessionPath
		}
		return ""
	case AgentTraeX, AgentAugureCode:
		return ""
	case AgentGrok:
		if filepath.Base(sessionPath) == "summary.json" {
			return filepath.Join(filepath.Dir(sessionPath), "subagents")
		}
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
	// fx, DeepSeek Harness, Antigravity CLI, and Evener children sit beside
	// their parents under one shared root. Each session keeps its own scope so an
	// archived child whose source is gone still gets its own search; Paths
	// lists the shared root only once per refresh.
	case AgentFx:
		if _, ok := fxSharedRoot(sessionPath); ok {
			return sessionPath
		}
		return ""
	case AgentDeepSeekHarness:
		if _, ok := deepSeekHarnessSharedRoot(sessionPath); ok {
			return sessionPath
		}
		return ""
	case AgentAntigravityCLI:
		if _, ok := antigravityCLISharedRoot(sessionPath); ok {
			return sessionPath
		}
		return ""
	case AgentGoose:
		if _, _, ok := ParseVirtualSourcePathForBase(sessionPath, GooseDBName); ok {
			return sessionPath
		}
		return ""
	case AgentCrush:
		if _, _, ok := ParseVirtualSourcePathForBase(sessionPath, CrushDBName); ok {
			return sessionPath
		}
		return ""
	case AgentEvener:
		if _, ok := evenerSharedRoot(sessionPath); ok {
			return sessionPath
		}
		return ""
	case AgentRooCode:
		if filepath.Base(sessionPath) == "history_item.json" && filepath.Base(filepath.Dir(filepath.Dir(sessionPath))) == "tasks" {
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
	case AgentCodex:
		return d.codexChildPaths(ctx, sessionPath, rawSessionID)
	case AgentGrok:
		return grokSubagentChildPaths(root)
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
		shared, _ := fxSharedRoot(sessionPath)
		return d.descendantPaths(agent, shared, rawSessionID, func() (parentLinkInventory, error) {
			return discoverFxSubagents(ctx, shared)
		})
	case AgentDeepSeekHarness:
		shared, _ := deepSeekHarnessSharedRoot(sessionPath)
		return d.descendantPaths(agent, shared, decodeDeepSeekHarnessCanonicalRawID(rawSessionID), func() (parentLinkInventory, error) {
			return discoverDeepSeekHarnessSubagents(ctx, shared)
		})
	case AgentAntigravityCLI:
		shared, _ := antigravityCLISharedRoot(sessionPath)
		return d.descendantPaths(agent, shared, rawSessionID, func() (parentLinkInventory, error) {
			return discoverAntigravityCLISubagents(ctx, shared)
		})
	case AgentGoose:
		dbPath, parentID, _ := ParseVirtualSourcePathForBase(sessionPath, GooseDBName)
		conn, err := openGooseDB(dbPath, false)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		columns, err := gooseTableColumns(ctx, conn, "sessions")
		if err != nil || !columns["parent_session_id"] {
			return nil, err
		}
		return sessionsTableDescendantPaths(ctx, conn, dbPath, parentID, GooseSQLiteVirtualPath)
	case AgentCrush:
		dbPath, parentID, _ := ParseVirtualSourcePathForBase(sessionPath, CrushDBName)
		conn, err := openCrushDB(dbPath, false)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		return sessionsTableDescendantPaths(ctx, conn, dbPath, parentID, VirtualSourcePath)
	case AgentEvener:
		shared, _ := evenerSharedRoot(sessionPath)
		return d.descendantPaths(agent, shared, rawSessionID, func() (parentLinkInventory, error) {
			return discoverEvenerSubagents(ctx, shared)
		})
	case AgentRooCode:
		paths, found, err := rooCodeChildPaths(sessionPath)
		if found || err != nil {
			return paths, err
		}
		// A retained task whose folder is gone is still named by its
		// children's parentTaskId.
		tasksDir := filepath.Dir(filepath.Dir(sessionPath))
		return d.descendantPaths(agent, tasksDir, rawSessionID, func() (parentLinkInventory, error) {
			return discoverRooCodeSubagents(ctx, filepath.Dir(tasksDir))
		})
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
	case AgentShelley, AgentMuse, AgentFx, AgentDeepSeekHarness, AgentAntigravityCLI, AgentEvener, AgentGoose, AgentCrush, AgentOmnigent:
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

// fxSharedRoot returns the sessions root of <root>/<id>/events.jsonl.
func fxSharedRoot(sessionPath string) (string, bool) {
	dir := filepath.Dir(sessionPath)
	if filepath.Base(sessionPath) != "events.jsonl" || !fxSessionIDValid(filepath.Base(dir)) {
		return "", false
	}
	return filepath.Dir(dir), true
}

// deepSeekHarnessSharedRoot returns the root of
// <root>/<project>/<id>/session*.jsonl[.zstd]. A child's project folder follows
// its own cwd, so discovery searches the whole root.
func deepSeekHarnessSharedRoot(sessionPath string) (string, bool) {
	root := filepath.Dir(filepath.Dir(filepath.Dir(sessionPath)))
	if _, _, _, ok := deepSeekHarnessPathParts(root, sessionPath); !ok {
		return "", false
	}
	return root, true
}

// antigravityCLISharedRoot returns the root of <root>/conversations/<id>.db
// or .pb and <root>/implicit/<id>.pb.
func antigravityCLISharedRoot(sessionPath string) (string, bool) {
	root := filepath.Dir(filepath.Dir(sessionPath))
	if _, ok := antigravityCLISessionIDForPath(root, sessionPath); !ok {
		return "", false
	}
	return root, true
}

// evenerSharedRoot returns the source root of
// <root>/sessions/<id>.transcript.jsonl or
// <root>/projects/<project>/sessions/<id>.transcript.jsonl. A child can live in
// another project, so discovery searches the whole root.
func evenerSharedRoot(sessionPath string) (string, bool) {
	sessions := filepath.Dir(sessionPath)
	if filepath.Base(sessions) != "sessions" {
		return "", false
	}
	root := filepath.Dir(sessions)
	if projects := filepath.Dir(root); filepath.Base(projects) == "projects" {
		root = filepath.Dir(projects)
	}
	if _, ok := evenerClassifyPath(root, sessionPath, true); !ok {
		return "", false
	}
	return root, true
}

// rooCodeChildPaths reads the parent's childIds. The parent records every
// child it spawned there, including children no newTask call names. found is
// false when the parent's history item is missing.
func rooCodeChildPaths(historyPath string) (paths []string, found bool, err error) {
	data, err := os.ReadFile(historyPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	var item rooCodeHistoryItem
	if err := json.Unmarshal(data, &item); err != nil {
		return nil, true, fmt.Errorf("decode Roo Code history item %s: %w", historyPath, err)
	}
	tasksDir := filepath.Dir(filepath.Dir(historyPath))
	for _, id := range item.ChildIDs {
		if !isSafeSinglePathComponent(id) {
			continue
		}
		if path := filepath.Join(tasksDir, id, "history_item.json"); IsRegularFile(path) {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	return paths, true, nil
}

// discoverRooCodeSubagents reads each task's parentTaskId.
func discoverRooCodeSubagents(ctx context.Context, root string) (parentLinkInventory, error) {
	inventory := parentLinkInventory{
		children: make(map[string][]string),
		paths:    make(map[string]string),
	}
	err := rooCodeDiscoverEach(ctx, root, func(match singleFileMatch) error {
		data, err := os.ReadFile(match.Path)
		if err != nil {
			return err
		}
		var item rooCodeHistoryItem
		if err := json.Unmarshal(data, &item); err != nil || item.ParentTaskID == "" {
			return nil //nolint:nilerr // Undecodable tasks are reported when the task itself syncs.
		}
		id := filepath.Base(filepath.Dir(match.Path))
		if item.ParentTaskID == id {
			return nil
		}
		inventory.paths[id] = match.Path
		inventory.children[item.ParentTaskID] = append(inventory.children[item.ParentTaskID], id)
		return nil
	})
	return inventory, err
}

// grokSubagentChildPaths reads <parent>/subagents/<child>/meta.json, where
// the parent records every child, including resumed and worktree children
// whose spawn returned no result. A worktree child lives under another cwd
// group, so each child is looked up across the whole sessions root.
func grokSubagentChildPaths(subagentsDir string) ([]string, error) {
	entries, err := os.ReadDir(subagentsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sessionDir := filepath.Dir(subagentsDir)
	sessionsRoot := filepath.Dir(filepath.Dir(sessionDir))
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() || !IsValidSessionID(entry.Name()) {
			continue
		}
		parentID, childID, ok := readGrokSubagentMeta(filepath.Join(subagentsDir, entry.Name(), "meta.json"))
		if !ok || parentID != filepath.Base(sessionDir) {
			continue
		}
		if childID == "" {
			childID = entry.Name()
		}
		if match, ok := grokFindFile(sessionsRoot, childID); ok {
			paths = append(paths, match.Path)
		}
	}
	slices.Sort(paths)
	return paths, nil
}

// codexDatedDay returns the day folder date of a local
// <root>/YYYY/MM/DD/rollout-*.jsonl path.
func codexDatedDay(sessionPath string) (time.Time, bool) {
	if strings.Contains(sessionPath, "://") || !isCodexSessionFilename(filepath.Base(sessionPath)) {
		return time.Time{}, false
	}
	dayDir := filepath.Dir(sessionPath)
	monthDir := filepath.Dir(dayDir)
	yearDir := filepath.Dir(monthDir)
	day, err := time.Parse("2006/01/02", filepath.Base(yearDir)+"/"+filepath.Base(monthDir)+"/"+filepath.Base(dayDir))
	return day, err == nil
}

// codexChildPaths finds rollouts whose session_meta names parentID.
// Guardian-review and similar children record their parent only there, so
// the parent never names them. A child starts after its parent, so the search
// covers the day folders from the parent's start through its last write,
// widened by a day on each side because folder dates use local time. The
// undated archived_sessions folder is not searched.
func (d *SubagentTranscriptDiscovery) codexChildPaths(ctx context.Context, sessionPath, parentID string) ([]string, error) {
	start, _ := codexDatedDay(sessionPath)
	end := start
	if info, err := os.Stat(sessionPath); err == nil {
		mtime := info.ModTime()
		if last := time.Date(mtime.Year(), mtime.Month(), mtime.Day(), 0, 0, 0, 0, time.UTC); last.After(end) {
			end = last
		}
	}
	sessionsRoot := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(sessionPath))))
	var paths []string
	for day := start.AddDate(0, 0, -1); !day.After(end.AddDate(0, 0, 1)); day = day.AddDate(0, 0, 1) {
		dir := filepath.Join(sessionsRoot, day.Format("2006"), day.Format("01"), day.Format("02"))
		found, err := d.descendantPaths(AgentCodex, dir, parentID, func() (parentLinkInventory, error) {
			return discoverCodexDaySubagents(ctx, dir)
		})
		if err != nil {
			return paths, err
		}
		paths = append(paths, found...)
	}
	slices.Sort(paths)
	return paths, nil
}

// discoverCodexDaySubagents reads the session_meta line of each rollout in
// one day folder.
func discoverCodexDaySubagents(ctx context.Context, dir string) (parentLinkInventory, error) {
	inventory := parentLinkInventory{
		children: make(map[string][]string),
		paths:    make(map[string]string),
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return inventory, nil
	}
	if err != nil {
		return inventory, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return inventory, err
		}
		if entry.IsDir() || !isCodexSessionFilename(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		id, parent := readCodexSessionMetaParent(ctx, path)
		if id == "" || parent == "" || parent == id {
			continue
		}
		inventory.paths[id] = path
		inventory.children[parent] = append(inventory.children[parent], id)
	}
	return inventory, nil
}

// readCodexSessionMetaParent decodes only a rollout's first line, its
// session_meta, and returns the thread ID and parent thread ID.
func readCodexSessionMetaParent(ctx context.Context, path string) (id, parentID string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	lr := newLineReaderContext(ctx, f, maxLineSize)
	defer releaseLineReader(lr)
	line, err := lr.readLineBytes()
	if err != nil || gjson.GetBytes(line, "type").Str != codexTypeSessionMeta {
		return "", ""
	}
	payload := gjson.GetBytes(line, "payload")
	return strings.TrimSpace(payload.Get("id").Str), codexSubagentParentThreadID(payload)
}

// discoverEvenerSubagents reads each session's metadata and transcript header.
// A parent names a child only in a finished delegate result, so a child that
// is still running is linked from its own records alone.
func discoverEvenerSubagents(ctx context.Context, root string) (parentLinkInventory, error) {
	inventory := parentLinkInventory{
		children: make(map[string][]string),
		paths:    make(map[string]string),
	}
	err := evenerDiscoverEach(ctx, root, func(match singleFileMatch) error {
		id := strings.TrimSuffix(filepath.Base(match.Path), evenerTranscriptSuffix)
		parent := readEvenerHeaderParentID(match.Path)
		if meta, _, err := readEvenerMeta(match.Path); err == nil && meta.ParentSessionID != "" {
			parent = meta.ParentSessionID
		}
		if parent == "" || parent == id || !evenerSafeID(parent) {
			return nil
		}
		inventory.paths[id] = match.Path
		inventory.children[parent] = append(inventory.children[parent], id)
		return nil
	})
	return inventory, err
}

// readEvenerHeaderParentID decodes only the transcript's first record.
func readEvenerHeaderParentID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	raw, err := jsontext.NewDecoder(f).ReadValue()
	if err != nil {
		return ""
	}
	var header evenerHeader
	if err := json.Unmarshal(raw, &header); err != nil || header.Kind != "header" {
		return ""
	}
	return header.ParentSessionID
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

// sessionsTableDescendantPaths follows sessions.parent_session_id, which
// Goose and Crush parent parsing never reads, through every level below
// parentID.
func sessionsTableDescendantPaths(
	ctx context.Context, conn *sql.DB, dbPath, parentID string,
	virtualPath func(dbPath, sessionID string) string,
) ([]string, error) {
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
			paths = append(paths, virtualPath(dbPath, id))
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

// discoverAntigravityCLISubagents reads agyReader.parentCascadeId from each
// session's trajectory sidecar, the only record of an Antigravity CLI parent
// link.
func discoverAntigravityCLISubagents(ctx context.Context, root string) (parentLinkInventory, error) {
	inventory := parentLinkInventory{
		children: make(map[string][]string),
		paths:    make(map[string]string),
	}
	for _, subdir := range []string{"conversations", "implicit"} {
		dir := filepath.Join(root, subdir)
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return inventory, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return inventory, err
			}
			id, ext, ok := antigravityCLIPathID(entry.Name())
			if entry.IsDir() || !ok {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if subdir == "conversations" && antigravityCLIConversationSource(root, id) != path {
				continue
			}
			storageID, ok := antigravityCLISessionIDForPath(root, path)
			if !ok {
				continue
			}
			parent := readAgyReaderParentCascadeID(strings.TrimSuffix(path, ext) + ".trajectory.json")
			if parent == "" || strings.EqualFold(parent, id) {
				continue
			}
			inventory.paths[storageID] = path
			inventory.children[parent] = append(inventory.children[parent], storageID)
		}
	}
	return inventory, nil
}
