package probe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testUser = "davuser"
	testPass = "davpass!"
)

func fakeFFProbe(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fake-ffprobe.sh"))
	if err != nil {
		t.Fatalf("resolve fake ffprobe: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fake ffprobe missing: %v", err)
	}
	return path
}

func captureArgs(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "argv.txt")
	t.Setenv("FFPROBE_CAPTURE", path)
	return path
}

func readCapture(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(lines) == 0 || lines[len(lines)-1] != "--END--" {
		t.Fatalf("capture has no --END-- marker: %q", string(body))
	}
	return lines[:len(lines)-1]
}

func validJSON(sourceURL, duration, size string) string {
	return fmt.Sprintf(`{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","duration":%q}],"format":{"filename":%q,"duration":%q,"size":%q}}`,
		duration, sourceURL, duration, size)
}

func basicAuth(user, pass string) string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestProbePassesRangeAndAuthToFFprobe(t *testing.T) {
	script := fakeFFProbe(t)
	capture := captureArgs(t)
	headerFile := filepath.Join(t.TempDir(), "headers.bin")
	t.Setenv("FFPROBE_HEADER_CAPTURE", headerFile)
	t.Setenv("FFPROBE_MODE", "json")
	sourceURL := "http://127.0.0.1:8090/webdav/movie.mkv"
	wantAuth := basicAuth(testUser, testPass)
	wantHeaders := wantAuth + "\r\nRange: bytes=0-\r\n"

	res, err := Probe(context.Background(), sourceURL, testUser, testPass, script, 45*time.Second, 45_000_000)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	args := readCapture(t, capture)
	wantPrefix := []string{
		"-v", "error",
		"-rw_timeout", "45000000",
		"-print_format", "json",
		"-show_format", "-show_streams", "-show_chapters",
		"-headers",
	}
	if len(args) != len(wantPrefix)+2 {
		t.Fatalf("got %d ffprobe args, want %d (argv not printed: carries the auth token)", len(args), len(wantPrefix)+2)
	}
	if !reflect.DeepEqual(args[:len(wantPrefix)], wantPrefix) {
		t.Errorf("ffprobe flags = %q, want %q", args[:len(wantPrefix)], wantPrefix)
	}
	headers := args[len(wantPrefix)]
	if got := args[len(args)-1]; got != sourceURL {
		t.Errorf("URL argument = %q, want %q", got, sourceURL)
	}

	// The fake writes the -headers argument verbatim. Assert the exact
	// CRLF-terminated lines: ffprobe splits on CRLF, so a missing terminator
	// after the Authorization line merges it into the Range line (401 + full
	// body). Failure messages never print the token.
	rawHeaders, err := os.ReadFile(headerFile)
	if err != nil {
		t.Fatalf("read raw headers: %v", err)
	}
	if string(rawHeaders) != wantHeaders {
		t.Errorf("ffprobe -headers value is not two CRLF-terminated lines (got %d bytes, want %d)", len(rawHeaders), len(wantHeaders))
	}
	lines := strings.Split(string(rawHeaders), "\r\n")
	if len(lines) != 3 || lines[0] != wantAuth || lines[1] != "Range: bytes=0-" || lines[2] != "" {
		t.Errorf("ffprobe -headers lines are malformed: got %d CRLF-separated parts, want Authorization / Range / empty", len(lines))
	}
	if !strings.Contains(headers, wantAuth) {
		t.Errorf("captured headers missing the Basic auth line")
	}
	if !strings.Contains(headers, "Range: bytes=0-") {
		t.Errorf("captured headers missing the Range line")
	}
	if strings.Contains(headers, testPass) || strings.Contains(headers, testUser+":"+testPass) {
		t.Errorf("captured headers contain plaintext credentials")
	}
	if len(res.JSON) == 0 || res.Digest == "" {
		t.Fatalf("empty result: %+v", res)
	}
	if len(res.Digest) != 64 {
		t.Errorf("digest = %q, want 64 hex chars", res.Digest)
	}
	var doc struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
		} `json:"streams"`
		Format struct {
			Filename string `json:"filename"`
		} `json:"format"`
	}
	if err := json.Unmarshal(res.JSON, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(doc.Streams) != 2 || doc.Streams[0].CodecName != "h264" {
		t.Errorf("ffprobe streams lost: %+v", doc.Streams)
	}
	if doc.Format.Filename != sourceURL {
		t.Errorf("filename = %q, want %q", doc.Format.Filename, sourceURL)
	}
}

func TestProbeRetriesSlowFirstByte(t *testing.T) {
	script := fakeFFProbe(t)
	capture := captureArgs(t)
	t.Setenv("FFPROBE_MODE", "http")

	var mu sync.Mutex
	attempts := 0
	body := validJSON("http://127.0.0.1:8090/webdav/movie.mkv", "95.5", "2048")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			// Altmount cold start: no first byte for 2s, then its 206 range
			// reply arrives after our per-attempt deadline.
			time.Sleep(2 * time.Second)
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte(body))
			return
		}
		// The retry gets an immediate ranged body. It is 200 rather than 206
		// only because the fake ffprobe uses busybox wget, which errors on 206.
		w.Header().Set("Content-Range", "bytes 0-511/2048")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	res, err := Probe(context.Background(), server.URL+"/webdav/movie.mkv", testUser, testPass, script, 900*time.Millisecond, 45_000_000)
	if err != nil {
		t.Fatalf("Probe after cold first byte: %v", err)
	}
	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 2 {
		t.Errorf("server saw %d requests, want 2 (cold attempt + retry)", got)
	}
	if markers := strings.Count(string(mustRead(t, capture)), "--END--"); markers != 2 {
		t.Errorf("ffprobe invoked %d times, want 2", markers)
	}
	var doc map[string]any
	if err := json.Unmarshal(res.JSON, &doc); err != nil {
		t.Fatalf("retried probe JSON invalid: %v", err)
	}
	format, ok := doc["format"].(map[string]any)
	if !ok || format["duration"] != "95.5" {
		t.Errorf("retried probe lost format: %v", doc["format"])
	}
}

func TestProbeTimesOutAfterOneRetry(t *testing.T) {
	script := fakeFFProbe(t)
	capture := captureArgs(t)
	t.Setenv("FFPROBE_MODE", "always-timeout")

	_, err := Probe(context.Background(), "http://127.0.0.1:9/webdav/movie.mkv", testUser, testPass, script, 100*time.Millisecond, 45_000_000)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "timed out") {
		t.Errorf("error = %q, want a timeout error", err)
	}
	if markers := strings.Count(string(mustRead(t, capture)), "--END--"); markers != 2 {
		t.Errorf("ffprobe invoked %d times, want exactly 2", markers)
	}
}

func TestProbeRetriesFFProbeRWTimeoutMessage(t *testing.T) {
	script := fakeFFProbe(t)
	capture := captureArgs(t)
	t.Setenv("FFPROBE_MODE", "timeout-error")

	// A generous outer timeout: ffprobe's own -rw_timeout fires first.
	_, err := Probe(context.Background(), "http://127.0.0.1:9/webdav/movie.mkv", testUser, testPass, script, 90*time.Second, 45_000_000)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "timed out") {
		t.Fatalf("error = %v, want a timeout error", err)
	}
	if markers := strings.Count(string(mustRead(t, capture)), "--END--"); markers != 2 {
		t.Errorf("ffprobe invoked %d times, want exactly 2", markers)
	}
}

func TestProbeDoesNotRetryInvalidData(t *testing.T) {
	script := fakeFFProbe(t)
	capture := captureArgs(t)
	t.Setenv("FFPROBE_MODE", "bad-json")

	_, err := Probe(context.Background(), "http://127.0.0.1:9/webdav/movie.mkv", testUser, testPass, script, time.Second, 45_000_000)
	if err == nil {
		t.Fatal("expected an invalid-JSON error")
	}
	if strings.Contains(strings.ToLower(err.Error()), "timed out") {
		t.Errorf("invalid data misclassified as timeout: %q", err)
	}
	if markers := strings.Count(string(mustRead(t, capture)), "--END--"); markers != 1 {
		t.Errorf("ffprobe invoked %d times for invalid data, want 1", markers)
	}
}

func TestProbeClampsRWTimeoutFloor(t *testing.T) {
	script := fakeFFProbe(t)
	capture := captureArgs(t)
	t.Setenv("FFPROBE_MODE", "json")

	if _, err := Probe(context.Background(), "http://127.0.0.1:9/webdav/movie.mkv", testUser, testPass, script, time.Second, 1_000_000); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	args := readCapture(t, capture)
	if len(args) < 4 || args[3] != "45000000" {
		t.Errorf("-rw_timeout = %q, want the 45,000,000us floor", args)
	}
}

func TestProbeSanitizesCredentialedURLs(t *testing.T) {
	script := fakeFFProbe(t)
	t.Setenv("FFPROBE_MODE", "echo-url")
	sourceURL := "http://alice:s3cr3t@127.0.0.1:8090/webdav/movie.mkv?token=abc123"
	cleanURL := "http://127.0.0.1:8090/webdav/movie.mkv"

	res, err := Probe(context.Background(), sourceURL, testUser, testPass, script, time.Second, 45_000_000)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	body := string(res.JSON)
	for i, secret := range []string{"alice", "s3cr3t", "token=abc123", "@", sourceURL} {
		if strings.Contains(body, secret) {
			t.Errorf("sanitized JSON leaked fixture secret #%d (value not printed)", i)
		}
	}
	var doc struct {
		Format struct {
			Filename string `json:"filename"`
			Tags     struct {
				Comment string `json:"comment"`
			} `json:"tags"`
		} `json:"format"`
		Streams []struct {
			Filename string `json:"filename"`
		} `json:"streams"`
		Chapters []struct {
			Tags struct {
				URL string `json:"url"`
			} `json:"tags"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal(res.JSON, &doc); err != nil {
		t.Fatalf("unmarshal sanitized JSON: %v", err)
	}
	if doc.Format.Filename != cleanURL {
		t.Errorf("format.filename = %q, want %q", doc.Format.Filename, cleanURL)
	}
	if want := "source: " + cleanURL; doc.Format.Tags.Comment != want {
		t.Errorf("nested comment = %q, want %q", doc.Format.Tags.Comment, want)
	}
	if len(doc.Streams) != 1 || doc.Streams[0].Filename != cleanURL {
		t.Errorf("stream filename not sanitised: %+v", doc.Streams)
	}
	if len(doc.Chapters) != 1 || doc.Chapters[0].Tags.URL != cleanURL {
		t.Errorf("chapter url not sanitised: %+v", doc.Chapters)
	}
}

func TestProbeErrorsNeverLeakCredentials(t *testing.T) {
	script := fakeFFProbe(t)
	captureArgs(t)
	t.Setenv("FFPROBE_MODE", "stderr-secret")
	secretHeader := basicAuth(testUser, testPass)
	t.Setenv("FFPROBE_SECRET_HEADER", secretHeader)
	sourceURL := "http://alice:s3cr3t@127.0.0.1:8090/webdav/movie.mkv?token=abc123"

	_, err := Probe(context.Background(), sourceURL, testUser, testPass, script, time.Second, 45_000_000)
	if err == nil {
		t.Fatal("expected ffprobe failure")
	}
	message := err.Error()
	for i, secret := range []string{"alice", "s3cr3t", "token=abc123", testUser, testPass, secretHeader, sourceURL} {
		if strings.Contains(message, secret) {
			t.Errorf("error leaked fixture secret #%d (value not printed)", i)
		}
	}
}

func TestScrubStringHandlesAnyCredentialedScheme(t *testing.T) {
	input := `source dav://user:pass@host/path?token=abc and http://u:p@example.com/f.json`
	got := scrubString(input, "")
	for i, secret := range []string{"user:pass", "token=abc", "u:p@", "pass@host"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubbed text leaked fixture secret #%d (value not printed)", i)
		}
	}
	if !strings.Contains(got, "dav://host/path") || !strings.Contains(got, "http://example.com/f.json") {
		t.Errorf("scrubbed text lost the URL paths: %q", got)
	}
}

func TestValidateRejectsZeroFill(t *testing.T) {
	res := Result{JSON: json.RawMessage(validJSON("http://dav/webdav/movie.mkv", "120.0", "1048576")), Digest: "digest"}
	reader := func(int64) ([]byte, error) { return make([]byte, 4096), nil }

	err := Validate(res, reader)
	if err == nil {
		t.Fatal("zero-filled samples were accepted")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "non-zero") {
		t.Errorf("error = %q, want a non-zero-sample explanation", err)
	}
}

func TestValidateRejectsNoStreams(t *testing.T) {
	res := Result{JSON: json.RawMessage(`{"streams":[],"format":{"duration":"10.0","size":"4096"}}`)}
	err := Validate(res, func(int64) ([]byte, error) { return []byte{1}, nil })
	if err == nil {
		t.Fatal("probe with no streams was accepted")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "stream") {
		t.Errorf("error = %q, want a stream explanation", err)
	}
}

func TestValidateAcceptsPlausibleProbe(t *testing.T) {
	res := Result{JSON: json.RawMessage(validJSON("http://dav/webdav/movie.mkv", "120.0", "1048576")), Digest: "digest"}
	offsets := make(map[int64]bool)
	reader := func(off int64) ([]byte, error) {
		offsets[off] = true
		if off < 0 {
			return nil, fmt.Errorf("negative offset")
		}
		return []byte{0x00, 0xAB, 0xCD}, nil
	}
	if err := Validate(res, reader); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(offsets) < 2 {
		t.Errorf("only sampled %d offsets, want at least 2", len(offsets))
	}
}

func TestValidateRejectsSingleNonZeroSample(t *testing.T) {
	res := Result{JSON: json.RawMessage(validJSON("http://dav/webdav/movie.mkv", "120.0", "1048576"))}
	first := true
	reader := func(int64) ([]byte, error) {
		if first {
			first = false
			return []byte{1, 2, 3}, nil
		}
		return []byte{0, 0, 0}, nil
	}
	if err := Validate(res, reader); err == nil {
		t.Fatal("only one non-zero offset was accepted")
	}
}

func TestValidateRejectsBadMetadata(t *testing.T) {
	cases := map[string]string{
		"unknown codec":    `{"streams":[{"codec_type":"video","codec_name":"notacodec","duration":"5"}],"format":{"duration":"5","size":"4096"}}`,
		"zero duration":    `{"streams":[{"codec_type":"video","codec_name":"h264","duration":"0"}],"format":{"duration":"0.000000","size":"4096"}}`,
		"missing duration": `{"streams":[{"codec_type":"video","codec_name":"h264"}],"format":{"size":"4096"}}`,
		"subtitle only":    `{"streams":[{"codec_type":"subtitle","codec_name":"h264","duration":"5"}],"format":{"duration":"5","size":"4096"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			res := Result{JSON: json.RawMessage(body)}
			if err := Validate(res, func(int64) ([]byte, error) { return []byte{1}, nil }); err == nil {
				t.Fatal("bad metadata was accepted")
			}
		})
	}
}

func TestValidateRejectsInvalidJSON(t *testing.T) {
	if err := Validate(Result{JSON: json.RawMessage("not json")}, func(int64) ([]byte, error) { return nil, nil }); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	return body
}
