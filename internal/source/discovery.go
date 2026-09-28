package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
	"golang.org/x/net/html"
)

// discoverLink finds the one download URL a page's match selects.
func (m *Manager) discoverLink(ctx context.Context, s nativeConfig) (link string, err error) {
	var host string
	if page, err := url.Parse(s.URL); err == nil {
		host = page.Hostname()
	}
	done := plugin.Stage(ctx, "Discovering source release", plugin.Detail(host))
	defer func() {
		found, _ := url.Parse(link)
		done(err, plugin.Detail(urlName(found)))
	}()
	req, err := m.request(ctx, s.URL, s)
	if err != nil {
		return "", err
	}
	res, err := m.Client.Do(req)
	if err != nil {
		return "", transportError("download page", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download page returned HTTP %d", res.StatusCode)
	}
	const limit = 4 << 20
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return "", transportError("download page", err)
	}
	if len(data) > limit {
		return "", errors.New("download page exceeds 4 MiB")
	}
	pattern, err := regexp.Compile(s.Match)
	if err != nil {
		return "", err
	}
	base := req.URL
	if res.Request != nil {
		base = res.Request.URL
	}
	// HTML carries links in element attributes; comments, text and script
	// content name URLs a browser never follows. Other pages are text.
	searched := []string{string(data)}
	if markup(res.Header.Get("Content-Type"), data) {
		searched, base = attributes(string(data), base)
	}
	matches := map[string]bool{}
	for _, text := range searched {
		for _, address := range pattern.FindAllString(text, -1) {
			reference, err := url.Parse(address)
			if err != nil || address == "" {
				return "", errors.New("download page match is not a valid URL reference")
			}
			resolved := base.ResolveReference(reference)
			address = resolved.String()
			if err := validateHTTPURL(address); err != nil {
				return "", fmt.Errorf("download page match must resolve to a stable HTTP(S) URL: %w", err)
			}
			matches[address] = true
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("download page matched %d distinct artifact URLs; expected one", len(matches))
	}
	for address := range matches {
		link = address
	}
	return link, nil
}

// markup reports whether a download page is HTML. Pages served without a
// declared type are sniffed the way a browser sniffs them.
func markup(declared string, page []byte) bool {
	media, _, err := mime.ParseMediaType(declared)
	if err != nil {
		media, _, _ = mime.ParseMediaType(http.DetectContentType(page))
	}
	return media == "text/html" || media == "application/xhtml+xml"
}

// attributes reports the decoded attribute values of an HTML page's elements
// and the base URL its references resolve against.
func attributes(page string, base *url.URL) ([]string, *url.URL) {
	var values []string
	document, located := base, false
	tokens := html.NewTokenizer(strings.NewReader(page))
	for {
		token := tokens.Next()
		if token == html.ErrorToken {
			return values, document
		}
		if token != html.StartTagToken && token != html.SelfClosingTagToken {
			continue
		}
		name, more := tokens.TagName()
		tag := string(name)
		// Stemma runs no scripts, so noscript content is ordinary markup.
		if tag == "noscript" {
			tokens.NextIsNotRawText()
		}
		for more {
			var key, value []byte
			key, value, more = tokens.TagAttr()
			values = append(values, string(value))
			if located || tag != "base" || string(key) != "href" {
				continue
			}
			if reference, err := url.Parse(strings.TrimSpace(string(value))); err == nil {
				document, located = base.ResolveReference(reference), true
			}
		}
	}
}
