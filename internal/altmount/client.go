// Package altmount enumerates the media files exposed by Altmount's WebDAV
// share. It reads file names only: no media bytes are fetched, stored or
// proxied here. Credentials are sent as HTTP Basic auth, never in a URL.
package altmount

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// webdavPrefix is where Altmount mounts its WebDAV share. A PROPFIND to
	// "/" is a 404 on this server, so a base missing this prefix is useless.
	webdavPrefix       = "/webdav/"
	defaultHTTPTimeout = 15 * time.Second
	propfindMethod     = "PROPFIND"
	propfindDepth      = "2"
)

// Client talks to one Altmount WebDAV share. NewClient is the only constructor;
// an invalid base is remembered and reported by List rather than hidden in a
// constructor that cannot return an error.
type Client struct {
	base     string // e.g. http://10.77.77.49:8090/webdav/
	basePath string // e.g. /webdav/
	user     string
	pass     string
	http     *http.Client
	err      error
}

// NewClient builds a client for a WebDAV base that must end in "/webdav/".
// A nil httpClient gets a bounded default. Credentials belong in the Basic
// auth header, so a base carrying userinfo is rejected.
func NewClient(base, user, pass string, httpClient *http.Client) *Client {
	c := &Client{user: user, pass: pass, http: httpClient}
	if c.http == nil {
		c.http = &http.Client{Timeout: defaultHTTPTimeout}
	}
	c.base, c.basePath, c.err = checkBase(base)
	return c
}

// checkBase validates a WebDAV base without ever echoing it: the value may
// carry credentials, which must not leak into an error or a log line.
func checkBase(raw string) (string, string, error) {
	if raw == "" {
		return "", "", fmt.Errorf("altmount: WebDAV base URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("altmount: WebDAV base must be an http(s) URL with a host and no credentials, query or fragment")
	}
	if !strings.HasSuffix(u.Path, webdavPrefix) {
		return "", "", fmt.Errorf("altmount: WebDAV base must end with %q", webdavPrefix)
	}
	// The path check above guarantees a trailing slash; normalise in case the
	// raw string had redundant ones after the host.
	return strings.TrimRight(raw, "/") + "/", u.Path, nil
}

// categories are the WebDAV folders this add-on enumerates. Whitelisting them
// keeps a caller-supplied category from becoming a path traversal.
var categories = map[string]bool{
	"movies":   true,
	"tv":       true,
	"complete": true,
}

func (c *Client) propfindURL(category string) (string, error) {
	if c.err != nil {
		return "", c.err
	}
	if !categories[category] {
		return "", fmt.Errorf("altmount: unknown WebDAV category %q", category)
	}
	return c.base + category + "/", nil
}
