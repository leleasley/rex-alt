// Package probe turns Altmount WebDAV file URLs into the ffprobe metadata REX
// requires. It only asks ffprobe for the bytes needed to probe (HTTP Range via
// the -rw_timeout/-headers options); it never downloads, stores or proxies
// media bytes. Credentials are passed in ffprobe's -headers option and are
// scrubbed from every URL-like value and every error before a Result escapes.
package probe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultProbeTimeout matches PROBE_TIMEOUT's default and floor in config.
	defaultProbeTimeout = 45 * time.Second
	// minRWTimeoutMicros is the ffprobe -rw_timeout floor: cold Altmount files
	// can take 13-24s to deliver their first byte.
	minRWTimeoutMicros = 45_000_000
	// retryBackoff is the wait before the single cold-start retry.
	retryBackoff = 2 * time.Second
	maxProbeJSON = 16 << 20
	maxErrorText = 240
)

// Result is the sanitised ffprobe document and its content digest. Digest is
// the hex SHA-256 of JSON and is what REX's probe_digest feature compares.
type Result struct {
	JSON   json.RawMessage
	Digest string
}

// Probe runs ffprobe against a WebDAV file URL once, retrying once only when
// the attempt times out (a cold/slow first byte). Invalid data is returned
// immediately and is never mistaken for a timeout. Basic auth is supplied via
// ffprobe's -headers option, never in the URL or in a logged command line.
func Probe(ctx context.Context, rawURL, user, pass, ffprobePath string, timeout time.Duration, rwTimeoutMicros int) (Result, error) {
	if strings.TrimSpace(rawURL) == "" {
		return Result{}, fmt.Errorf("probe: WebDAV URL is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	if rwTimeoutMicros < minRWTimeoutMicros {
		rwTimeoutMicros = minRWTimeoutMicros
	}
	if ffprobePath == "" {
		ffprobePath = "ffprobe"
	}
	auth := basicHeader(user, pass)
	// Range: bytes=0- makes Altmount answer with 206 and lets ffprobe read only
	// the header bytes it needs; no whole-file read is ever requested here.
	headers := auth + "Range: bytes=0-\r\n"

	var lastErr string
	for attempt := 0; attempt < 2; attempt++ {
		if attempt == 1 {
			select {
			case <-time.After(retryBackoff):
			case <-ctx.Done():
				return Result{}, fmt.Errorf("probe: cancelled during retry: %w", ctx.Err())
			}
		}

		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		stdout, stderr, runErr := runFFProbe(attemptCtx, ffprobePath, rwTimeoutMicros, headers, rawURL)
		deadlineHit := attemptCtx.Err() == context.DeadlineExceeded
		cancel()

		if runErr != nil {
			// A timeout is a slow first byte, not bad media. An ffprobe that
			// fails late (just before our deadline) is too: retry it once.
			// A fast failure is invalid data and must not be retried.
			if (deadlineHit || elapsedNearDeadline(start, timeout) || looksLikeTimeout(stderr)) && ctx.Err() == nil {
				lastErr = safeErrorText(stderr, rawURL, user, pass, auth, runErr)
				continue
			}
			return Result{}, fmt.Errorf("probe: ffprobe failed: %s", safeErrorText(stderr, rawURL, user, pass, auth, runErr))
		}
		if deadlineHit {
			// ffprobe produced output but our deadline fired; treat it as a
			// cold read rather than persisting a partial document.
			lastErr = "probe exceeded its timeout"
			continue
		}

		clean, err := sanitizeJSON(stdout, rawURL)
		if err != nil {
			return Result{}, err
		}
		sum := sha256.Sum256(clean)
		return Result{JSON: clean, Digest: fmt.Sprintf("%x", sum[:])}, nil
	}
	return Result{}, fmt.Errorf("probe: timed out after one retry: %s", lastErr)
}

func runFFProbe(ctx context.Context, path string, rwTimeoutMicros int, headers, rawURL string) ([]byte, string, error) {
	// The pinned flag order is part of the probe contract. Never log args:
	// the -headers value carries the Basic-auth token.
	args := []string{
		"-v", "error",
		"-rw_timeout", strconv.Itoa(rwTimeoutMicros),
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		"-show_chapters",
		"-headers", headers,
		rawURL,
	}
	cmd := exec.CommandContext(ctx, path, args...)
	// A killed ffprobe can leave child processes holding the output pipes;
	// WaitDelay stops us waiting for orphans (the fake ffprobe sleeps in tests,
	// real ffprobe may fork helpers).
	cmd.WaitDelay = 500 * time.Millisecond
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.String(), err
}

func basicHeader(user, pass string) string {
	token := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	// ffprobe splits -headers on CRLF. Without the terminator the next header
	// is concatenated onto this line, yielding a garbage auth token and no
	// Range header (which would allow a full-body response).
	return "Authorization: Basic " + token + "\r\n"
}

func elapsedNearDeadline(start time.Time, timeout time.Duration) bool {
	return !start.IsZero() && time.Since(start) >= timeout*3/4
}

// looksLikeTimeout catches an ffprobe that gave up on its own -rw_timeout
// before our (possibly longer) configured PROBE_TIMEOUT expired.
func looksLikeTimeout(stderr string) bool {
	lower := strings.ToLower(stderr)
	for _, marker := range []string{"timed out", "timeout", "deadline exceeded"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// Validate rejects probe data that describes a zero-filled or truncated file.
// It needs a plausible stream, a positive duration, and at least two non-zero
// samples at distinct offsets. The reader is expected to perform small ranged
// GETs; Validate itself never reads a whole file.
func Validate(result Result, sampleReader func(offset int64) ([]byte, error)) error {
	if len(result.JSON) == 0 {
		return fmt.Errorf("probe: empty probe result")
	}
	var doc struct {
		Format struct {
			Duration string `json:"duration"`
			Size     string `json:"size"`
		} `json:"format"`
		Streams []struct {
			CodecType string          `json:"codec_type"`
			CodecName string          `json:"codec_name"`
			Duration  json.RawMessage `json:"duration"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(result.JSON, &doc); err != nil {
		return fmt.Errorf("probe: invalid probe JSON: %w", err)
	}

	known := false
	for _, stream := range doc.Streams {
		if stream.CodecType == "video" || stream.CodecType == "audio" {
			if knownCodecs[strings.ToLower(strings.TrimSpace(stream.CodecName))] {
				known = true
				break
			}
		}
	}
	if !known {
		return fmt.Errorf("probe: no video or audio stream with a known codec")
	}

	duration, ok := parseNumber(doc.Format.Duration)
	if !ok || duration <= 0 || math.IsInf(duration, 0) || math.IsNaN(duration) {
		for _, stream := range doc.Streams {
			if d, ok := parseRawNumber(stream.Duration); ok && d > 0 && !math.IsInf(d, 0) && !math.IsNaN(d) {
				duration, ok = d, true
				break
			}
		}
	}
	if !ok || duration <= 0 || math.IsInf(duration, 0) || math.IsNaN(duration) {
		return fmt.Errorf("probe: implausible duration")
	}

	if sampleReader == nil {
		return fmt.Errorf("probe: no sample reader")
	}
	size, _ := strconv.ParseInt(strings.TrimSpace(doc.Format.Size), 10, 64)
	nonZero, seen := 0, make(map[int64]bool)
	for _, offset := range sampleOffsets(size) {
		if seen[offset] {
			continue
		}
		seen[offset] = true
		sample, err := sampleReader(offset)
		if err != nil || len(sample) == 0 {
			continue
		}
		for _, b := range sample {
			if b != 0 {
				nonZero++
				break
			}
		}
	}
	if nonZero < 2 {
		return fmt.Errorf("probe: fewer than 2 non-zero samples at different offsets; file may be zero-filled")
	}
	return nil
}

// knownCodecs is the allow-list ffprobe codec_name values accepted for the
// visibility gate. Unknown names are rejected instead of trusted.
var knownCodecs = map[string]bool{
	// video
	"h264": true, "hevc": true, "h265": true, "av1": true, "vp8": true,
	"vp9": true, "mpeg4": true, "mpeg2video": true, "mpeg1video": true,
	"vc1": true, "wmv3": true, "prores": true, "huffyuv": true,
	"ffv1": true, "rawvideo": true, "theora": true, "dirac": true,
	// audio
	"aac": true, "ac3": true, "eac3": true, "flac": true, "mp3": true,
	"opus": true, "vorbis": true, "dts": true, "truehd": true, "alac": true,
	"pcm_s16le": true, "pcm_s24le": true, "pcm_s32le": true, "pcm_f32le": true,
}

func sampleOffsets(size int64) []int64 {
	if size > 0 {
		step := size / 4
		if step < 1 {
			step = 1
		}
		offsets := []int64{step, 2 * step, 3 * step}
		for i, off := range offsets {
			if off >= size {
				offsets[i] = size - 1
			}
		}
		return offsets
	}
	return []int64{1 << 20, 4 << 20, 16 << 20}
}

func parseNumber(raw string) (float64, bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	return value, err == nil
}

func parseRawNumber(raw json.RawMessage) (float64, bool) {
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		return number, true
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, false
	}
	return parseNumber(text)
}

// sanitizeJSON recursively strips userinfo, query and fragment from every
// URL-like value (including URLs embedded in free text). All other ffprobe
// fields are preserved; the document is re-marshalled with normalised spacing.
func sanitizeJSON(body []byte, rawURL string) ([]byte, error) {
	if len(body) > maxProbeJSON {
		return nil, fmt.Errorf("probe: ffprobe output exceeds %d bytes", maxProbeJSON)
	}
	var document any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("probe: ffprobe returned invalid JSON")
	}
	clean, err := json.Marshal(scrubURLs(document, rawURL))
	if err != nil {
		return nil, fmt.Errorf("probe: cannot encode ffprobe JSON: %w", err)
	}
	return clean, nil
}

func scrubURLs(value any, rawURL string) any {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			v[key] = scrubURLs(child, rawURL)
		}
	case []any:
		for i, child := range v {
			v[i] = scrubURLs(child, rawURL)
		}
	case string:
		return scrubString(v, rawURL)
	}
	return value
}

// Any scheme is matched, not just http(s): ffprobe can echo dav:// or other
// provider URLs, and all of them must lose userinfo/query before storage.
var urlPattern = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]+`)

func scrubString(value, rawURL string) string {
	if rawURL != "" && strings.Contains(value, rawURL) {
		value = strings.ReplaceAll(value, rawURL, cleanURL(rawURL))
	}
	return urlPattern.ReplaceAllStringFunc(value, cleanURLWithTail)
}

func cleanURLWithTail(match string) string {
	tail := ""
	for len(match) > 0 && strings.ContainsRune(".,;)]}", rune(match[len(match)-1])) {
		tail = string(match[len(match)-1]) + tail
		match = match[:len(match)-1]
	}
	return cleanURL(match) + tail
}

func cleanURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	if u.User == nil && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery {
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.ForceQuery = false
	return u.String()
}

// safeErrorText scrubs the source URL, the Basic-auth token and any other
// credentialed URL out of ffprobe stderr before it can reach a log or caller.
func safeErrorText(stderr, rawURL, user, pass, auth string, runErr error) string {
	text := strings.TrimSpace(stderr)
	if text == "" && runErr != nil {
		text = runErr.Error()
	}
	secrets := []string{auth}
	if token := strings.TrimSpace(strings.TrimPrefix(auth, "Authorization: Basic ")); token != "" {
		secrets = append(secrets, token)
	}
	if user != "" || pass != "" {
		secrets = append(secrets, user+":"+pass)
	}
	if rawURL != "" {
		secrets = append(secrets, rawURL)
	}
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "<redacted>")
		}
	}
	text = urlPattern.ReplaceAllStringFunc(text, cleanURLWithTail)
	lines := strings.Split(strings.TrimSpace(text), "\n")
	text = strings.TrimSpace(lines[len(lines)-1])
	if len(text) > maxErrorText {
		text = text[:maxErrorText] + "..."
	}
	return text
}
