package rex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	testUser = "alice"
	testPass = "s3cret"
)

func TestRegisterSendsCredentialsOutsideURL(t *testing.T) {
	const publicURL = "http://media.example:8080/files"
	dirs := []Directory{
		{Name: "movies", Movies: "Movies", Shows: "TV Shows"},
		{Name: "anime", Shows: "Anime"},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		if r.URL.Path != "/rex/sources/rex-alt" {
			t.Errorf("path = %q, want /rex/sources/rex-alt", r.URL.Path)
		}
		if r.URL.User != nil {
			t.Error("request URL carried userinfo")
		}

		var body struct {
			URL         string      `json:"url"`
			User        string      `json:"user"`
			Password    string      `json:"password"`
			Mount       string      `json:"mount"`
			Trash       *bool       `json:"trash"`
			Directories []Directory `json:"directories"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode registration: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.URL != publicURL {
			t.Errorf("url = %q, want %q", body.URL, publicURL)
		}
		if parsed, err := url.Parse(body.URL); err != nil || parsed.User != nil {
			t.Errorf("registered url contains userinfo or is invalid: %q (%v)", body.URL, err)
		}
		if body.User != testUser || body.Password != testPass {
			t.Errorf("credentials in wrong fields: user=%q password=%q", body.User, body.Password)
		}
		if body.Mount != "/altmount" {
			t.Errorf("mount = %q, want /altmount", body.Mount)
		}
		if body.Trash == nil || *body.Trash {
			t.Errorf("trash = %v, want false", body.Trash)
		}
		if len(body.Directories) != len(dirs) {
			t.Errorf("got %d directories, want %d", len(body.Directories), len(dirs))
		} else {
			for i := range dirs {
				if body.Directories[i] != dirs[i] {
					t.Errorf("directory %d = %+v, want %+v", i, body.Directories[i], dirs[i])
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"rex-alt","features":["probe_digest"]}`))
	}))
	defer server.Close()

	c := NewClient(server.URL, "", testUser, testPass, 100)
	features, err := c.Register(context.Background(), publicURL, dirs)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !features.ProbeDigest {
		t.Fatal("ProbeDigest = false, want true")
	}
}

func TestPushBatchesAt100AndRejectsOversize(t *testing.T) {
	var mu sync.Mutex
	var counts []int
	var sizes []int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/rex/feed/rex-alt/items" {
			t.Errorf("unexpected push request: %s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read push body: %v", err)
			return
		}
		var payload struct {
			Items []Item `json:"items"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode push body: %v", err)
		}
		if len(body) >= 16<<20 {
			t.Errorf("push body is %d bytes, must be under 16 MiB", len(body))
		}
		mu.Lock()
		counts = append(counts, len(payload.Items))
		sizes = append(sizes, len(body))
		mu.Unlock()
		_, _ = w.Write([]byte(fmt.Sprintf(`{"created":%d}`, len(payload.Items))))
	}))
	defer server.Close()

	c := NewClient(server.URL, "", testUser, testPass, 100)
	items := make([]Item, 250)
	for i := range items {
		items[i] = Item{
			ID:        fmt.Sprintf("movies/Release/file%03d.mkv", i),
			Path:      fmt.Sprintf("movies/Release/file%03d.mkv", i),
			MediaInfo: json.RawMessage(`{"streams":[]}`),
		}
	}
	result, err := c.Push(context.Background(), items)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if result.Created != 250 {
		t.Errorf("Created = %d, want 250", result.Created)
	}
	mu.Lock()
	gotCounts := append([]int(nil), counts...)
	gotSizes := append([]int(nil), sizes...)
	mu.Unlock()
	if len(gotCounts) != 3 || gotCounts[0] != 100 || gotCounts[1] != 100 || gotCounts[2] != 50 {
		t.Errorf("batch sizes = %v, want [100 100 50]", gotCounts)
	}
	for _, size := range gotSizes {
		if size >= 16<<20 {
			t.Errorf("push body is %d bytes, must be under 16 MiB", size)
		}
	}

	t.Run("single oversized item is rejected", func(t *testing.T) {
		mu.Lock()
		before := len(counts)
		mu.Unlock()
		big := Item{
			ID:        "movies/Release/huge.mkv",
			Path:      "movies/Release/huge.mkv",
			MediaInfo: json.RawMessage(`{"blob":"` + strings.Repeat("x", 16<<20) + `"}`),
		}
		_, err := c.Push(context.Background(), []Item{big})
		if err == nil {
			t.Fatal("Push with a 16 MiB item succeeded, want an error")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "limit") {
			t.Errorf("error %q does not explain the body limit", err)
		}
		if strings.Contains(err.Error(), testPass) || strings.Contains(err.Error(), testUser) {
			t.Errorf("error echoed credentials: %q", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(counts) != before {
			t.Errorf("oversized item was sent to rex")
		}
	})

	t.Run("large batch is split under the cap", func(t *testing.T) {
		mu.Lock()
		counts = nil
		mu.Unlock()
		var large []Item
		for i := 0; i < 4; i++ {
			large = append(large, Item{
				ID:        fmt.Sprintf("movies/Large/part%d.mkv", i),
				Path:      fmt.Sprintf("movies/Large/part%d.mkv", i),
				MediaInfo: json.RawMessage(`{"blob":"` + strings.Repeat("y", 5<<20) + `"}`),
			})
		}
		result, err := c.Push(context.Background(), large)
		if err != nil {
			t.Fatalf("Push of splittable batch: %v", err)
		}
		if result.Created != 4 {
			t.Errorf("Created = %d, want 4", result.Created)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(counts) < 2 {
			t.Errorf("large batch was not split: %v", counts)
		}
		total := 0
		for _, n := range counts {
			total += n
		}
		if total != 4 {
			t.Errorf("split batches carried %d items, want 4", total)
		}
	})
}

func TestDeleteSendsIDs(t *testing.T) {
	var mu sync.Mutex
	var batches [][]string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/rex/feed/rex-alt/items" {
			t.Errorf("unexpected delete request: %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			IDs []string `json:"ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode delete body: %v", err)
		}
		mu.Lock()
		batches = append(batches, body.IDs)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"trashed":1,"deleted":2,"released":3,"unknown":4}`))
	}))
	defer server.Close()

	c := NewClient(server.URL, "", testUser, testPass, 100)
	ids := make([]string, 250)
	for i := range ids {
		ids[i] = fmt.Sprintf("movies/Release/file%03d.mkv", i)
	}
	result, err := c.Delete(context.Background(), ids)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if result.Deleted != 6 || result.Trashed != 3 || result.Released != 9 || result.Unknown != 12 {
		t.Errorf("result = %+v, want deleted=6 trashed=3 released=9 unknown=12", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 3 {
		t.Fatalf("got %d delete requests, want 3", len(batches))
	}
	for i, want := range []int{100, 100, 50} {
		if len(batches[i]) != want {
			t.Errorf("delete batch %d has %d ids, want %d", i, len(batches[i]), want)
		}
		if want == 100 {
			for j, id := range batches[i] {
				if id != ids[i*100+j] {
					t.Errorf("delete batch %d id %d = %q, want %q", i, j, id, ids[i*100+j])
					break
				}
			}
		}
	}
}

func TestFeaturesParsedFromRegistration(t *testing.T) {
	cases := []struct {
		name     string
		response string
		want     bool
	}{
		{"advertised", `{"features":["probe_digest"]}`, true},
		{"among others", `{"features":["other","probe_digest"]}`, true},
		{"absent", `{}`, false},
		{"empty", `{"features":[]}`, false},
		{"unrelated", `{"features":["something_else"]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.response))
			}))
			defer server.Close()
			c := NewClient(server.URL, "rex-alt", testUser, testPass, 100)
			features, err := c.Register(context.Background(), "http://media.example/files", nil)
			if err != nil {
				t.Fatalf("Register: %v", err)
			}
			if features.ProbeDigest != tc.want {
				t.Errorf("ProbeDigest = %v, want %v", features.ProbeDigest, tc.want)
			}
		})
	}
}

func TestErrorsNeverEchoCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"rejected ` + testUser + `:` + testPass + `"}`))
	}))
	defer server.Close()
	c := NewClient(server.URL, "", testUser, testPass, 100)
	ctx := context.Background()

	calls := map[string]func() error{
		"register": func() error {
			_, err := c.Register(ctx, "http://media.example/files", nil)
			return err
		},
		"push": func() error {
			_, err := c.Push(ctx, []Item{{ID: "id", Path: "path", MediaInfo: json.RawMessage(`{}`)}})
			return err
		},
		"delete": func() error {
			_, err := c.Delete(ctx, []string{"id"})
			return err
		},
		"source": func() error {
			_, err := c.Source(ctx)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatal("expected an error")
			}
			msg := err.Error()
			if strings.Contains(msg, testUser) || strings.Contains(msg, testPass) {
				t.Errorf("error echoed credentials: %q", msg)
			}
			if !strings.Contains(msg, "***") {
				t.Errorf("error did not redact credentials: %q", msg)
			}
		})
	}
}

func TestHealthyAndSource(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/identity" {
				t.Errorf("path = %q, want /identity", r.URL.Path)
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()
		c := NewClient(server.URL, "", testUser, testPass, 100)
		if err := c.Healthy(context.Background()); err != nil {
			t.Fatalf("Healthy: %v", err)
		}
	})

	t.Run("unhealthy", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()
		c := NewClient(server.URL, "", testUser, testPass, 100)
		if err := c.Healthy(context.Background()); err == nil {
			t.Fatal("Healthy returned nil for HTTP 503")
		}
	})

	t.Run("source found and missing", func(t *testing.T) {
		var registered atomic.Bool
		registered.Store(true)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/rex/sources/rex-alt" {
				t.Errorf("path = %q, want /rex/sources/rex-alt", r.URL.Path)
			}
			if !registered.Load() {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"name":"rex-alt","auth":"basic","features":["probe_digest"]}`))
		}))
		defer server.Close()
		c := NewClient(server.URL, "", testUser, testPass, 100)
		source, err := c.Source(context.Background())
		if err != nil || source["name"] != "rex-alt" {
			t.Fatalf("Source = %v, %v", source, err)
		}
		registered.Store(false)
		source, err = c.Source(context.Background())
		if err != nil || source != nil {
			t.Fatalf("missing Source = %v, %v; want nil, nil", source, err)
		}
	})
}

func TestNewClientCapsBatchAndStripsUserInfo(t *testing.T) {
	var mu sync.Mutex
	var count, userinfo int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.User != nil {
			mu.Lock()
			userinfo++
			mu.Unlock()
		}
		var payload struct {
			Items []Item `json:"items"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		count++
		mu.Unlock()
		_, _ = w.Write([]byte(fmt.Sprintf(`{"created":%d}`, len(payload.Items))))
	}))
	defer server.Close()

	// A configured batch above 100 must still respect the feed's batch cap.
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.User = url.UserPassword(testUser, testPass)
	c := NewClient(parsed.String(), "custom-source", testUser, testPass, 500)
	items := make([]Item, 250)
	for i := range items {
		items[i] = Item{ID: fmt.Sprintf("id%d", i), Path: fmt.Sprintf("path%d", i), MediaInfo: json.RawMessage(`{}`)}
	}
	result, err := c.Push(context.Background(), items)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if result.Created != 250 {
		t.Errorf("Created = %d, want 250", result.Created)
	}
	mu.Lock()
	defer mu.Unlock()
	if count != 3 {
		t.Errorf("got %d push requests, want 3", count)
	}
	if userinfo != 0 {
		t.Errorf("%d request URLs carried userinfo", userinfo)
	}
}
