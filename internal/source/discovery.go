package source

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
	"golang.org/x/net/html"
)

// dottedVersion is what a match's version group may capture.
var dottedVersion = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)*$`)

// discoverLink finds the one download URL a page's match selects. A pattern
// with a version group may match several; the highest version selects the
// URL and is returned with it.
func (m *Manager) discoverLink(ctx context.Context, s httpConfig) (link, version string, err error) {
	var host string
	if page, err := url.Parse(s.URL); err == nil {
		host = page.Hostname()
	}
	done := plugin.Stage(ctx, "Discovering source release", plugin.Detail(host))
	defer func() {
		found, _ := url.Parse(link)
		done(err, plugin.Detail(urlName(found)))
	}()
	req, err := m.request(ctx, s.URL, s.headers(s.URL))
	if err != nil {
		return "", "", err
	}
	res, err := m.metadataClient().Do(req)
	if err != nil {
		return "", "", transportError("download page", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", "", httpStatusError("download page", req, res)
	}
	const limit = 4 << 20
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return "", "", transportError("download page", err)
	}
	if len(data) > limit {
		return "", "", errors.New("download page exceeds 4 MiB")
	}
	pattern, err := regexp.Compile(s.Match)
	if err != nil {
		return "", "", err
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
	group := pattern.SubexpIndex("version")
	matches := map[string]string{}
	for _, text := range searched {
		for _, match := range pattern.FindAllStringSubmatch(text, -1) {
			reference, err := url.Parse(match[0])
			if err != nil || match[0] == "" {
				return "", "", errors.New("download page match is not a valid URL reference")
			}
			address := base.ResolveReference(reference).String()
			if err := validateHTTPURL(address); err != nil {
				return "", "", fmt.Errorf("download page match must resolve to a stable HTTP(S) URL: %w", err)
			}
			captured := ""
			if group >= 0 {
				captured = match[group]
				if !dottedVersion.MatchString(captured) || len(captured) > 64 {
					return "", "", fmt.Errorf("download page match captured version %q; a version group captures dotted numbers", captured)
				}
			}
			if known, ok := matches[address]; !ok || compareDotted(captured, known) > 0 {
				matches[address] = captured
			}
		}
	}
	// Without a version group every capture is empty and all URLs tie.
	var selected []string
	for _, address := range slices.Sorted(maps.Keys(matches)) {
		order := 1
		if len(selected) > 0 {
			order = compareDotted(matches[address], matches[selected[0]])
		}
		switch {
		case order > 0:
			selected = []string{address}
		case order == 0:
			selected = append(selected, address)
		}
	}
	switch {
	case len(selected) == 1:
		return selected[0], matches[selected[0]], nil
	case group >= 0 && len(selected) > 1:
		return "", "", fmt.Errorf("download page matched %d distinct artifact URLs for version %s; expected one%s", len(selected), matches[selected[0]], matched(selected))
	}
	return "", "", fmt.Errorf("download page matched %d distinct artifact URLs; expected one%s", len(selected), matched(selected))
}

// matched lists the first URLs of an ambiguous match.
func matched(addresses []string) string {
	const shown = 5
	switch {
	case len(addresses) == 0:
		return ""
	case len(addresses) > shown:
		return fmt.Sprintf(": %s and %d more", strings.Join(addresses[:shown], ", "), len(addresses)-shown)
	}
	return ": " + strings.Join(addresses, ", ")
}

// compareDotted orders dotted numeric versions of any length. Missing
// components compare as zero, so 6.3 and 6.3.0 are equal.
func compareDotted(a, b string) int {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(left), len(right)) {
		var x, y string
		if i < len(left) {
			x = strings.TrimLeft(left[i], "0")
		}
		if i < len(right) {
			y = strings.TrimLeft(right[i], "0")
		}
		if order := cmp.Or(cmp.Compare(len(x), len(y)), strings.Compare(x, y)); order != 0 {
			return order
		}
	}
	return 0
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
