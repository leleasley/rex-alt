package altmount

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/leleasley/rex-alt/internal/models"
)

const (
	testDAVUser = "usenet"
	testDAVPass = "dav-secret"
	propfind    = "PROPFIND"
)

// davFixture is the repo-level DAV fixture. Go runs each package's tests with
// the package directory as the working directory, hence the relative path.
func davFixture(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "dav_root.xml"))
	if err != nil {
		t.Fatalf("read DAV fixture: %v", err)
	}
	return body
}

// wantID computes the expected id independently of library.ItemID: ids are
// the readable WebDAV-relative path.
func wantID(e models.Entry) string {
	return strings.Join([]string{e.Category, e.Release, e.File}, "/")
}

func TestListParsesPROPFIND207(t *testing.T) {
	fixture := davFixture(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != propfind {
			t.Errorf("method = %q, want PROPFIND", r.Method)
		}
		if r.URL.Path != "/webdav/movies/" {
			t.Errorf("path = %q, want /webdav/movies/", r.URL.Path)
		}
		if got := r.Header.Get("Depth"); got != "infinity" {
			t.Errorf("Depth = %q, want infinity (Altmount rejects 2 with HTTP 400)", got)
		}
		if r.URL.User != nil {
			t.Error("request URL carried userinfo; credentials belong in Basic auth")
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != testDAVUser || pass != testDAVPass {
			t.Errorf("Basic auth = (%q, %q, %v), want configured credentials", user, pass, ok)
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	c := NewClient(server.URL+"/webdav/", testDAVUser, testDAVPass, server.Client())
	entries, err := c.List(context.Background(), "movies")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("got %d PROPFIND requests, want 1", got)
	}
	wantPaths := []string{"movies/Rel/file.mkv", "movies/Rel/second.mp4"}
	if len(entries) != len(wantPaths) {
		t.Fatalf("got %d entries (%+v), want %d: subtitles, NFOs, samples and nested files must be skipped", len(entries), entries, len(wantPaths))
	}
	for i, e := range entries {
		if e.Path != wantPaths[i] {
			t.Errorf("entry %d path = %q, want %q", i, e.Path, wantPaths[i])
		}
		if e.Category != "movies" || e.Release != "Rel" {
			t.Errorf("entry %d category/release = %q/%q, want movies/Rel", i, e.Category, e.Release)
		}
		if e.File != strings.TrimPrefix(wantPaths[i], "movies/Rel/") {
			t.Errorf("entry %d file = %q", i, e.File)
		}
		if e.ID != wantID(e) {
			t.Errorf("entry %d id = %q, want %q", i, e.ID, wantID(e))
		}
		if e.Season != 0 || e.Episode != 0 {
			t.Errorf("entry %d season/episode = %d/%d, want 0/0 (Task 7 parses these)", i, e.Season, e.Episode)
		}
	}
}

// TestPropfindNeverUsesDepthTwo records a live Altmount finding: Depth: 2 gets
// HTTP 400, while Depth: 0/1/infinity answer 207 and infinity is the only depth
// that includes files below a release folder. The fake server mimics the 400,
// so a regression to "2" fails the test instead of silently passing locally.
func TestPropfindNeverUsesDepthTwo(t *testing.T) {
	var depthTwo atomic.Int32
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.Header.Get("Depth"); got == "2" {
			depthTwo.Add(1)
			w.WriteHeader(http.StatusBadRequest) // exactly what Altmount returns
			return
		} else if got != "infinity" {
			t.Errorf("Depth = %q, want infinity", got)
		}
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`<d:multistatus xmlns:d="DAV:"/>`))
	}))
	defer server.Close()

	c := NewClient(server.URL+"/webdav/", testDAVUser, testDAVPass, server.Client())
	entries, err := c.List(context.Background(), "tv")
	if err != nil {
		t.Fatalf("List: %v; a Depth: 2 regression gets HTTP 400 on real Altmount", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %v entries, want none", entries)
	}
	if got := depthTwo.Load(); got != 0 {
		t.Errorf("client sent Depth: 2 %d time(s); Altmount rejects it with HTTP 400", got)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("got %d PROPFIND requests, want 1 (no retry on 400)", got)
	}
}

func TestListRejectsMissingTrailingSlashBase(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`<d:multistatus xmlns:d="DAV:"/>`))
	}))
	defer server.Close()

	cases := []struct {
		name string
		base string
	}{
		{"no webdav prefix", server.URL},
		{"no trailing slash", server.URL + "/webdav"},
		{"wrong prefix", server.URL + "/dav/"},
		{"category as base", server.URL + "/webdav/movies/"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient(tc.base, testDAVUser, testDAVPass, server.Client())
			entries, err := c.List(context.Background(), "movies")
			if err == nil {
				t.Fatalf("List with base %q returned %v, want an error", tc.base, entries)
			}
			if entries != nil {
				t.Errorf("List returned %v alongside an error, want nil", entries)
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("invalid bases made %d HTTP requests, want 0", got)
	}
}

func TestListUsesCategoryPathAndDecodesHrefs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/webdav/tv/" {
			t.Errorf("path = %q, want /webdav/tv/", r.URL.Path)
		}
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`<?xml version="1.0"?>` +
			`<d:multistatus xmlns:d="DAV:">` +
			`<d:response><d:href>/webdav/tv/Some%20Show/</d:href></d:response>` +
			`<d:response><d:href>/webdav/tv/Some%20Show/Episode%201.mkv</d:href></d:response>` +
			`<d:response><d:href>` + `http://altmount.invalid/webdav/tv/Some%20Show/Episode%202.mp4` + `</d:href></d:response>` +
			`</d:multistatus>`))
	}))
	defer server.Close()

	c := NewClient(server.URL+"/webdav/", testDAVUser, testDAVPass, server.Client())
	entries, err := c.List(context.Background(), "tv")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"tv/Some Show/Episode 1.mkv", "tv/Some Show/Episode 2.mp4"}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries (%+v), want %d", len(entries), entries, len(want))
	}
	for i, e := range entries {
		if e.Path != want[i] {
			t.Errorf("entry %d path = %q, want %q", i, e.Path, want[i])
		}
		if e.Release != "Some Show" {
			t.Errorf("entry %d release = %q, want Some Show", i, e.Release)
		}
	}
}

func TestListRejectsBadResponsesWithoutEchoingCredentials(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, `denied for ` + testDAVUser + `:` + testDAVPass},
		{"not found", http.StatusNotFound, ``},
		{"malformed xml", http.StatusMultiStatus, `<d:multistatus xmlns:d="DAV:"><d:response>`},
		{"wrong root", http.StatusMultiStatus, `<html><body>not a multistatus</body></html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			c := NewClient(server.URL+"/webdav/", testDAVUser, testDAVPass, server.Client())
			entries, err := c.List(context.Background(), "movies")
			if err == nil {
				t.Fatalf("List returned %v, want an error", entries)
			}
			msg := err.Error()
			if strings.Contains(msg, testDAVUser) || strings.Contains(msg, testDAVPass) {
				t.Errorf("error echoed credentials: %q", msg)
			}
		})
	}
}

func TestListRejectsInvalidCategory(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`<d:multistatus xmlns:d="DAV:"/>`))
	}))
	defer server.Close()
	c := NewClient(server.URL+"/webdav/", testDAVUser, testDAVPass, server.Client())
	for _, category := range []string{"", "../movies", "movies/../tv", "/movies", "movies/extra"} {
		if _, err := c.List(context.Background(), category); err == nil {
			t.Errorf("List(%q) succeeded, want an error", category)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("invalid categories made %d HTTP requests, want 0", got)
	}
}

func TestListUsesDefaultHTTPClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:"/>`)
	}))
	defer server.Close()
	c := NewClient(server.URL+"/webdav/", testDAVUser, testDAVPass, nil)
	if c.http == nil {
		t.Fatal("NewClient(nil httpClient) left a nil HTTP client")
	}
	entries, err := c.List(context.Background(), "movies")
	if err != nil || len(entries) != 0 {
		t.Errorf("List = %v, %v; want empty catalogue, nil error", entries, err)
	}
}
