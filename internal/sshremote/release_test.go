package sshremote

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testTarballName = "agentsview_0.45.0_linux_amd64.tar.gz"

var testTarballBytes = []byte("pretend tarball bytes")

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// releaseServer serves one version's tarball and SHA256SUMS. A nil sums or
// tarball means that path answers 404.
func releaseServer(t *testing.T, tarball, sums []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	if tarball != nil {
		mux.HandleFunc("/v0.45.0/"+testTarballName, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(tarball)
		})
	}
	if sums != nil {
		mux.HandleFunc("/v0.45.0/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(sums)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func sumsLine(hash, name string) []byte {
	return []byte(fmt.Sprintf("%s  %s\n", hash, name))
}

func TestReleaseFetcherDownloadsAndCaches(t *testing.T) {
	sums := append(
		sumsLine(sha256Hex([]byte("other")), "agentsview_0.45.0_darwin_arm64.tar.gz"),
		sumsLine(sha256Hex(testTarballBytes), testTarballName)...)
	srv := releaseServer(t, testTarballBytes, sums)
	cache := t.TempDir()
	f := releaseFetcher{BaseURL: srv.URL, CacheDir: cache}

	path, err := f.Fetch(t.Context(), "0.45.0", platform{"linux", "amd64"})

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(cache, "0.45.0", testTarballName), path)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, testTarballBytes, got)

	srv.Close()
	again, err := f.Fetch(t.Context(), "0.45.0", platform{"linux", "amd64"})
	require.NoError(t, err)
	assert.Equal(t, path, again)
}

func TestReleaseFetcherRedownloadsCorruptedCache(t *testing.T) {
	srv := releaseServer(t, testTarballBytes,
		sumsLine(sha256Hex(testTarballBytes), testTarballName))
	f := releaseFetcher{BaseURL: srv.URL, CacheDir: t.TempDir()}
	path, err := f.Fetch(t.Context(), "0.45.0", platform{"linux", "amd64"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("tampered"), 0o600))

	path, err = f.Fetch(t.Context(), "0.45.0", platform{"linux", "amd64"})

	require.NoError(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, testTarballBytes, got)
}

func TestReleaseFetcherFailures(t *testing.T) {
	goodSums := sumsLine(sha256Hex(testTarballBytes), testTarballName)
	tests := []struct {
		name         string
		tarball      []byte
		sums         []byte
		wantNotFound bool
		wantContains string
	}{
		{
			name:         "checksum mismatch",
			tarball:      testTarballBytes,
			sums:         sumsLine(sha256Hex([]byte("different")), testTarballName),
			wantContains: "checksum",
		},
		{
			name:    "missing SHA256SUMS entry",
			tarball: testTarballBytes,
			sums: sumsLine(sha256Hex(testTarballBytes),
				"agentsview_0.45.0_darwin_arm64.tar.gz"),
			wantContains: testTarballName,
		},
		{
			name:         "partial name does not match",
			tarball:      testTarballBytes,
			sums:         sumsLine(sha256Hex(testTarballBytes), "x"+testTarballName),
			wantContains: testTarballName,
		},
		{name: "tarball 404", tarball: nil, sums: goodSums, wantNotFound: true},
		{name: "SHA256SUMS 404", tarball: testTarballBytes, sums: nil, wantNotFound: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := releaseServer(t, tt.tarball, tt.sums)
			cache := t.TempDir()
			f := releaseFetcher{BaseURL: srv.URL, CacheDir: cache}

			path, err := f.Fetch(t.Context(), "0.45.0", platform{"linux", "amd64"})

			require.Error(t, err)
			assert.Empty(t, path)
			if tt.wantNotFound {
				require.ErrorIs(t, err, errReleaseNotFound)
			} else {
				require.NotErrorIs(t, err, errReleaseNotFound)
				assert.Contains(t, err.Error(), tt.wantContains)
			}
			entries, err := os.ReadDir(filepath.Join(cache, "0.45.0"))
			require.NoError(t, err)
			for _, e := range entries {
				assert.Equal(t, "SHA256SUMS", e.Name(), "unexpected leftover file")
			}
		})
	}
}

func TestReleaseFetcherServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	f := releaseFetcher{BaseURL: srv.URL, CacheDir: t.TempDir()}

	_, err := f.Fetch(t.Context(), "0.45.0", platform{"linux", "amd64"})

	require.Error(t, err)
	require.NotErrorIs(t, err, errReleaseNotFound)
	assert.Contains(t, err.Error(), "500")
}

func TestReleaseFetcherConcurrentFetchesShareCache(t *testing.T) {
	const fetchers = 8
	var tarballHits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v0.45.0/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(sumsLine(sha256Hex(testTarballBytes), testTarballName))
	})
	mux.HandleFunc("/v0.45.0/"+testTarballName, func(w http.ResponseWriter, _ *http.Request) {
		tarballHits.Add(1)
		_, _ = w.Write(testTarballBytes)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// Two SSH hosts on one platform can provision at once in a daemon.
	f := releaseFetcher{BaseURL: srv.URL, CacheDir: t.TempDir()}

	var wg sync.WaitGroup
	paths := make([]string, fetchers)
	errs := make([]error, fetchers)
	for i := range fetchers {
		wg.Go(func() {
			paths[i], errs[i] = f.Fetch(t.Context(), "0.45.0", platform{"linux", "amd64"})
		})
	}
	wg.Wait()

	for i := range fetchers {
		require.NoError(t, errs[i])
		got, err := os.ReadFile(paths[i])
		require.NoError(t, err)
		assert.Equal(t, testTarballBytes, got)
	}
	assert.Equal(t, int64(1), tarballHits.Load(), "one download serves every fetch")
	entries, err := os.ReadDir(filepath.Dir(paths[0]))
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{"SHA256SUMS", testTarballName}, names)
}
