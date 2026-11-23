package altmount

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxPROPFINDBytes bounds a multistatus reply so a broken server cannot exhaust
// memory. Even the largest Altmount category listing is far below this.
const maxPROPFINDBytes = 64 << 20

// propfindBody asks only for the properties enumeration needs. The response's
// hrefs carry the file paths; properties are ignored by the parser.
const propfindBody = `<?xml version="1.0" encoding="utf-8"?>
<propfind xmlns="DAV:"><prop><resourcetype/><getcontentlength/><getlastmodified/></prop></propfind>`

// multiStatus matches both default-DAV and prefixed (<d:multistatus>) replies:
// an XMLName tag without a namespace matches any namespace.
type multiStatus struct {
	XMLName   xml.Name      `xml:"multistatus"`
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href string `xml:"href"`
}

// propfind issues PROPFIND <target> with Depth: 2 and Basic auth, and returns
// the decoded response hrefs. A non-2xx status or a non-multistatus body is an
// error; the body is never echoed because it could contain credentials.
func (c *Client) propfind(ctx context.Context, target string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, propfindMethod, target, strings.NewReader(propfindBody))
	if err != nil {
		return nil, fmt.Errorf("altmount: build PROPFIND: %w", err)
	}
	req.Header.Set("Depth", propfindDepth)
	req.Header.Set("Content-Type", `application/xml; charset=utf-8`)
	req.SetBasicAuth(c.user, c.pass)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("altmount: PROPFIND: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("altmount: PROPFIND returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPROPFINDBytes+1))
	if err != nil {
		return nil, fmt.Errorf("altmount: read multistatus: %w", err)
	}
	if len(body) > maxPROPFINDBytes {
		return nil, fmt.Errorf("altmount: multistatus exceeds %d bytes", maxPROPFINDBytes)
	}

	var ms multiStatus
	if err := xml.Unmarshal(body, &ms); err != nil {
		return nil, fmt.Errorf("altmount: decode multistatus: %w", err)
	}
	hrefs := make([]string, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		if r.Href != "" {
			hrefs = append(hrefs, r.Href)
		}
	}
	return hrefs, nil
}

// hrefPath decodes a WebDAV href, which may be an absolute path ("/webdav/...")
// or an absolute URL ("http://host/webdav/..."). url.Parse percent-decodes the
// path component, so release names with spaces and unicode work verbatim.
func hrefPath(href string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return "", err
	}
	if u.Path == "" {
		return "", fmt.Errorf("altmount: href has no path")
	}
	return u.Path, nil
}
