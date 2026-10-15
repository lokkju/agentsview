package sshremote

import (
	"archive/tar"
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// serveTestConfig returns a config whose only Claude directory is a fresh
// temp dir, and that directory.
func serveTestConfig(t *testing.T) (config.Config, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	claudeDir := filepath.Join(root, "claude-projects")
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentClaude: {claudeDir},
	}}
	return cfg, claudeDir
}

// writeServeTestSession writes one Claude session under dir and returns its
// path.
func writeServeTestSession(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "project-a", "session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		testjsonl.NewSessionBuilder().
			AddClaudeUser("2024-01-01T00:00:00Z", "serve test").
			String(),
	), 0o644))
	return path
}

func encodeServeRequest(t *testing.T, request serveRequest) *bytes.Buffer {
	t.Helper()
	var in bytes.Buffer
	require.NoError(t, json.MarshalWrite(&in, request))
	return &in
}

func decodeServeResponse(t *testing.T, out io.Reader) []byte {
	t.Helper()
	zr, err := remotesync.NewZstdReader(out)
	require.NoError(t, err)
	defer zr.Close()
	body, err := io.ReadAll(zr)
	require.NoError(t, err)
	return body
}

func TestServe(t *testing.T) {
	cfg, claudeDir := serveTestConfig(t)
	writeServeTestSession(t, claudeDir)
	allowed := remotesync.TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {claudeDir}},
	}
	outside := remotesync.TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {t.TempDir()}},
	}
	version := remotesync.ProtocolVersion
	archiveOf := func(targets remotesync.TargetSet) *remotesync.ArchiveRequest {
		return &remotesync.ArchiveRequest{TargetSet: targets}
	}

	tests := []struct {
		name     string
		op       Op
		request  serveRequest
		wantCode ErrorCode // empty means success
	}{
		{"targets", OpTargets, serveRequest{ProtocolVersion: version}, ""},
		{
			"wrong protocol", OpTargets,
			serveRequest{ProtocolVersion: version + 1},
			CodeIncompatibleProtocol,
		},
		{"manifest", OpManifest, serveRequest{ProtocolVersion: version, Targets: &allowed}, ""},
		{
			"manifest outside roots", OpManifest,
			serveRequest{ProtocolVersion: version, Targets: &outside},
			CodeForbidden,
		},
		{
			"manifest without targets", OpManifest,
			serveRequest{ProtocolVersion: version},
			CodeInternal,
		},
		{
			"archive", OpArchive,
			serveRequest{ProtocolVersion: version, Archive: archiveOf(allowed)},
			"",
		},
		{
			"archive outside roots", OpArchive,
			serveRequest{ProtocolVersion: version, Archive: archiveOf(outside)},
			CodeForbidden,
		},
		{"archive without body", OpArchive, serveRequest{ProtocolVersion: version}, CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := encodeServeRequest(t, tt.request)
			var out bytes.Buffer

			err := Serve(t.Context(), tt.op, cfg, in, &out)

			if tt.wantCode != "" {
				serveErr, ok := errors.AsType[*ServeError](err)
				require.True(t, ok, "error %v is not a ServeError", err)
				assert.Equal(t, tt.wantCode, serveErr.Code)
				return
			}
			require.NoError(t, err)
			assert.NotEmpty(t, decodeServeResponse(t, &out))
		})
	}
}

func TestServeRejectsMalformedRequest(t *testing.T) {
	cfg, _ := serveTestConfig(t)
	var out bytes.Buffer

	err := Serve(t.Context(), OpTargets, cfg, strings.NewReader("{not json"), &out)

	serveErr, ok := errors.AsType[*ServeError](err)
	require.True(t, ok, "error %v is not a ServeError", err)
	assert.Equal(t, CodeInternal, serveErr.Code)
	assert.Empty(t, out.Bytes(), "a refused request must not start a response")
}

func TestServeTargetsReturnsResolvedTargets(t *testing.T) {
	cfg, claudeDir := serveTestConfig(t)
	writeServeTestSession(t, claudeDir)
	in := encodeServeRequest(t, serveRequest{ProtocolVersion: remotesync.ProtocolVersion})
	var out bytes.Buffer

	require.NoError(t, Serve(t.Context(), OpTargets, cfg, in, &out))

	var targets remotesync.TargetSet
	require.NoError(t, json.Unmarshal(decodeServeResponse(t, &out), &targets))
	assert.Equal(t, []string{claudeDir}, targets.Dirs[parser.AgentClaude])
}

func TestServeArchiveStreamsSessionTar(t *testing.T) {
	cfg, claudeDir := serveTestConfig(t)
	sessionPath := writeServeTestSession(t, claudeDir)
	in := encodeServeRequest(t, serveRequest{
		ProtocolVersion: remotesync.ProtocolVersion,
		Archive: &remotesync.ArchiveRequest{
			Dirs: map[parser.AgentType][]string{parser.AgentClaude: {claudeDir}},
		},
	})
	var out bytes.Buffer

	require.NoError(t, Serve(t.Context(), OpArchive, cfg, in, &out))

	tr := tar.NewReader(bytes.NewReader(decodeServeResponse(t, &out)))
	var names []string
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		names = append(names, header.Name)
	}
	assert.Contains(t, names, strings.TrimPrefix(sessionPath, "/"))
}

func TestClassifyServeError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ErrorCode
	}{
		{
			"target not allowed",
			fmt.Errorf("plan: %w", remotesync.ErrTargetNotAllowed), CodeForbidden,
		},
		{"file not allowed", fmt.Errorf("plan: %w", remotesync.ErrFileNotAllowed), CodeForbidden},
		{
			"manifest unsupported",
			fmt.Errorf("manifest: %w", remotesync.ErrManifestUnsupported), CodeUnsupported,
		},
		{"other failure", errors.New("disk full"), CodeInternal},
		{
			"serve error kept",
			&ServeError{Code: CodeIncompatibleProtocol, Message: "old"}, CodeIncompatibleProtocol,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, classifyServeError(tt.err).Code)
		})
	}
}
