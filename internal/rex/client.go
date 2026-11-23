// Package rex is the HTTP client for REX's source registration and feed API.
// It deliberately keeps credentials out of every URL: they are sent only in
// the registration body's user/password fields, and are redacted from errors.
package rex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultSourceName = "rex-alt"
	maxBatch          = 100
	maxBodyBytes      = 16 << 20 // REX refuses a feed body of 16 MiB or more.
	splitBodyBytes    = 15 << 20 // Split before that limit, leaving headroom.
	messageLimit      = 512
	redacted          = "***"
	probeDigest       = "probe_digest"
)

// Client talks to one REX server as one registered source.
type Client struct {
	base       string
	sourceName string
	user       string
	pass       string
	batch      int
	http       *http.Client
}

// Directory declares a library REX should make for the first directory of an
// item's path. Shows and movies are the library titles REX should use.
type Directory struct {
	Name   string `json:"name"`
	Movies string `json:"movies,omitempty"`
	Shows  string `json:"shows,omitempty"`
}

// Features is what the REX that answered a registration understands. A REX
// without probe_digest must not receive items whose probe stands in for their
// media_info.
type Features struct {
	ProbeDigest bool
}

// Result aggregates the counters REX answers to a feed request. Rejected and
// Wants come from upserts; Deleted, Trashed, Released and Unknown from deletes.
type Result struct {
	Created   int         `json:"created"`
	Updated   int         `json:"updated"`
	Held      int         `json:"held"`
	Unchanged int         `json:"unchanged"`
	Rejected  []Rejection `json:"rejected"`
	Wants     []string    `json:"wants"`
	Deleted   int         `json:"deleted"`
	Trashed   int         `json:"trashed"`
	Released  int         `json:"released"`
	Unknown   int         `json:"unknown"`
}

// Rejection is one item REX refused, with its reason.
type Rejection struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// NewClient builds a client for base. An empty source name becomes "rex-alt";
// the batch is clamped to 1..100 so no feed request exceeds the API's batch of
// 100 items. Any userinfo in base is discarded: credentials belong in the
// registration body, never in a request URL.
func NewClient(base, source, user, pass string, batch int) *Client {
	base = strings.TrimRight(base, "/")
	if parsed, err := url.Parse(base); err == nil && parsed.User != nil {
		parsed.User = nil
		base = strings.TrimRight(parsed.String(), "/")
	}
	if source == "" {
		source = defaultSourceName
	}
	if batch < 1 || batch > maxBatch {
		batch = maxBatch
	}
	return &Client{
		base:       base,
		sourceName: source,
		user:       user,
		pass:       pass,
		batch:      batch,
		http:       &http.Client{Timeout: 60 * time.Second},
	}
}

// Healthy checks that REX answers its identity endpoint.
func (c *Client) Healthy(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/identity", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("rex health: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("rex health: HTTP %d: %s", resp.StatusCode, c.safeMessage(resp.Body))
	}
	return nil
}

// Register registers (or re-registers) this source with REX. publicURL is the
// address REX will fetch bytes from; its credentials are sent separately in
// user/password. The returned Features describe what this REX supports.
func (c *Client) Register(ctx context.Context, publicURL string, dirs []Directory) (Features, error) {
	if err := checkPublicURL(publicURL); err != nil {
		return Features{}, err
	}
	body := map[string]any{
		"url":         strings.TrimRight(publicURL, "/"),
		"user":        c.user,
		"password":    c.pass,
		"trash":       false,
		"directories": dirs,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Features{}, fmt.Errorf("marshal registration: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/rex/sources/"+c.sourceName, bytes.NewReader(payload))
	if err != nil {
		return Features{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Features{}, fmt.Errorf("register with rex: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return Features{}, fmt.Errorf("register with rex: HTTP %d: %s", resp.StatusCode, c.safeMessage(resp.Body))
	}
	var answer struct {
		Features []string `json:"features"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return Features{}, fmt.Errorf("decode registration: %w", err)
	}
	var features Features
	for _, feature := range answer.Features {
		if feature == probeDigest {
			features.ProbeDigest = true
		}
	}
	return features, nil
}

// Source returns REX's view of this source, or nil when it is not registered.
// REX never answers credentials, only their kind.
func (c *Client) Source(ctx context.Context) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/rex/sources/"+c.sourceName, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rex source: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("rex source: HTTP %d: %s", resp.StatusCode, c.safeMessage(resp.Body))
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode rex source: %w", err)
	}
	return out, nil
}

// Push upserts items in batches of at most 100. A batch whose marshalled body
// would reach the 16 MiB API cap is split further until each request fits; a
// single item too large to fit is reported as an error rather than sent.
func (c *Client) Push(ctx context.Context, items []Item) (Result, error) {
	var total Result
	for start := 0; start < len(items); start += c.batch {
		end := min(start+c.batch, len(items))
		chunks, err := chunkPayloads(items[start:end], func(batch []Item) ([]byte, error) {
			return json.Marshal(map[string]any{"items": batch})
		})
		if err != nil {
			return total, err
		}
		for _, payload := range chunks {
			result, err := c.postItems(ctx, payload)
			if err != nil {
				return total, err
			}
			addResult(&total, result)
		}
	}
	return total, nil
}

// Delete removes the given item ids, in batches of at most 100.
func (c *Client) Delete(ctx context.Context, ids []string) (Result, error) {
	var total Result
	for start := 0; start < len(ids); start += c.batch {
		end := min(start+c.batch, len(ids))
		chunks, err := chunkPayloads(ids[start:end], func(batch []string) ([]byte, error) {
			return json.Marshal(map[string]any{"ids": batch})
		})
		if err != nil {
			return total, err
		}
		for _, payload := range chunks {
			result, err := c.deleteItems(ctx, payload)
			if err != nil {
				return total, err
			}
			addResult(&total, result)
		}
	}
	return total, nil
}

func (c *Client) postItems(ctx context.Context, payload []byte) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.feedURL("items"), bytes.NewReader(payload))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("push to rex: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return Result{}, fmt.Errorf("push to rex: HTTP %d: %s", resp.StatusCode, c.safeMessage(resp.Body))
	}
	var result Result
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return Result{}, fmt.Errorf("decode push result: %w", err)
	}
	return result, nil
}

func (c *Client) deleteItems(ctx context.Context, payload []byte) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.feedURL("items"), bytes.NewReader(payload))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("delete from rex: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return Result{}, fmt.Errorf("delete from rex: HTTP %d: %s", resp.StatusCode, c.safeMessage(resp.Body))
	}
	var result Result
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return Result{}, fmt.Errorf("decode delete result: %w", err)
	}
	return result, nil
}

func (c *Client) feedURL(what string) string {
	return c.base + "/rex/feed/" + c.sourceName + "/" + what
}

// safeMessage bounds a response body and removes any credential that a server
// might echo back in an error, so an error can never leak them into logs.
func (c *Client) safeMessage(body io.Reader) string {
	message := readMessage(body)
	for _, secret := range []string{c.pass, c.user} {
		if secret == "" {
			continue
		}
		for _, variant := range []string{secret, url.QueryEscape(secret)} {
			message = strings.ReplaceAll(message, variant, redacted)
		}
	}
	return message
}

func readMessage(body io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(body, messageLimit))
	message := strings.TrimSpace(string(raw))
	if message == "" {
		return "no response body"
	}
	return message
}

// checkPublicURL refuses anything REX would refuse, without echoing the value:
// a URL with userinfo would put credentials where REX answers them back.
func checkPublicURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("rex: public URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return fmt.Errorf("rex: public URL must be http(s) with a host and no credentials")
	}
	return nil
}

// chunkPayloads marshals values and, when the result would reach the API's
// body cap, splits them in half recursively until every body fits. Halving
// always makes progress for len >= 2; one oversized value cannot be sent.
func chunkPayloads[T any](values []T, marshal func([]T) ([]byte, error)) ([][]byte, error) {
	if len(values) == 0 {
		return nil, nil
	}
	payload, err := marshal(values)
	if err != nil {
		return nil, fmt.Errorf("marshal feed request: %w", err)
	}
	if len(payload) <= splitBodyBytes {
		return [][]byte{payload}, nil
	}
	if len(values) == 1 {
		return nil, fmt.Errorf("rex feed: a single item exceeds the %d byte body limit", maxBodyBytes)
	}
	mid := len(values) / 2
	left, err := chunkPayloads(values[:mid], marshal)
	if err != nil {
		return nil, err
	}
	right, err := chunkPayloads(values[mid:], marshal)
	if err != nil {
		return nil, err
	}
	return append(left, right...), nil
}

func addResult(total *Result, part Result) {
	total.Created += part.Created
	total.Updated += part.Updated
	total.Held += part.Held
	total.Unchanged += part.Unchanged
	total.Rejected = append(total.Rejected, part.Rejected...)
	total.Wants = append(total.Wants, part.Wants...)
	total.Deleted += part.Deleted
	total.Trashed += part.Trashed
	total.Released += part.Released
	total.Unknown += part.Unknown
}
