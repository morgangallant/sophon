package sophon

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/html/charset"
)

// Page is a parsed HTML page.
// TODO currently minimal, just HREFs + some simple metadata
type Page struct {
	Title     string
	Links     []*url.URL
	Canonical *url.URL
	NoFollow  bool
	NoIndex   bool
}

// Parses a fetched resource into a Page structure.
// The returned Page structure will include other URLs that
// the page links to, via <a href> tags.
func parseResource(resource *Resource, maxLinks int) (*Page, error) {
	// Can't assume UTF-8, need to detect the charset first and convert
	// before parsing the HTML.
	rdr, err := charset.NewReader(bytes.NewReader(resource.Content), resource.ContentType)
	if err != nil {
		return nil, fmt.Errorf("failed to create charset reader: %w", err)
	}

	root, err := html.Parse(rdr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse HTML: %w", err)
	}

	// Links are resolved against this base URL
	base := resource.URL

	// The found HREF URLs
	// Parsed later, since base could change
	var hrefs []string

	// Whether title/base have been set yet
	// Subsequent <title> or <base> tags will be ignored
	var (
		titleSet bool
		baseSet  bool
	)

	// The canonical HREF, resolved later (base could change)
	var canonicalHref string

	// Walk the page, looking for HREFs, the title, and other metadata.
	page := &Page{}
	for n := range root.Descendants() {
		if n.Type != html.ElementNode || n.Namespace != "" {
			continue // skip text nodes, and title/a tags within <svg> or <math>
		}
		switch n.DataAtom {
		case atom.A, atom.Area:
			// Add any <a href> URLs that are not nofollow
			href, ok := getAttr(n, "href")
			if !ok || hasRel(n, "nofollow") {
				continue
			}
			hrefs = append(hrefs, href)
		case atom.Base:
			// HTML pages can set a <base href> element, which
			// sets the base URL for relative links.
			if baseSet {
				continue
			}
			href, ok := getAttr(n, "href")
			if !ok {
				continue
			}
			if u, ok := resolveHref(resource.URL, href); ok {
				base = u
				baseSet = true
			}
		case atom.Link:
			// HTML pages can include a <link rel="canonical" ..>
			// to tell search engines which URL to use for the page.
			if canonicalHref != "" || !hasRel(n, "canonical") {
				continue
			}
			if href, ok := getAttr(n, "href"); ok {
				canonicalHref = href
			}
		case atom.Meta:
			// Respect noindex/nofollow tags that are added in
			// <meta> tags. TODO we might eventually want to add
			// support for targeted <meta> tags (e.g. specifically
			// for the "sophon" crawler or other name).
			//
			// TODO the same directives can also be sent in an X-Robots-Tag
			// HTTP header, which we don't see here since Resource doesn't
			// keep headers. Store it in the fetcher and apply it here too.
			//
			// TODO handle <meta http-equiv="refresh" content="0; url=/new">,
			// which is an HTML-level redirect. Parse the url= part and add it
			// to hrefs (or a separate Redirect field, to treat it like a 3xx).
			name, _ := getAttr(n, "name")
			if !strings.EqualFold(name, "robots") {
				continue
			}
			content, _ := getAttr(n, "content")
			for _, directive := range strings.Split(content, ",") {
				switch strings.ToLower(strings.TrimSpace(directive)) {
				case "noindex":
					page.NoIndex = true
				case "nofollow":
					page.NoFollow = true
				case "none":
					page.NoIndex = true
					page.NoFollow = true
				}
			}
		case atom.Title:
			// While we're parsing the tree, might as well
			// take this opportunity to extract the page title,
			// if it is set.
			if titleSet {
				continue
			}
			var sb strings.Builder
			for c := range n.ChildNodes() {
				if c.Type == html.TextNode {
					sb.WriteString(c.Data)
				}
			}
			page.Title = strings.Join(strings.Fields(sb.String()), " ")
			titleSet = true
		}
	}

	// Resolve the canonical URL, if one was found
	if canonicalHref != "" {
		if u, ok := resolveHref(base, canonicalHref); ok {
			page.Canonical = u
		}
	}

	// If the whole page is marked nofollow, don't
	// return _any_ URLs, to avoid the caller accidentally
	// following a link it shouldn't.
	if page.NoFollow {
		hrefs = nil
	}

	// Resolve the URLs from the base, and deduplicate
	seen := make(map[string]struct{})
	for _, href := range hrefs {
		if len(page.Links) >= maxLinks {
			break
		}
		u, ok := resolveHref(base, href)
		if !ok {
			continue
		}
		key := normalizeURL(u).String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		page.Links = append(page.Links, u)
	}

	return page, nil
}

func getAttr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func hasRel(n *html.Node, value string) bool {
	rel, _ := getAttr(n, "rel")
	for _, v := range strings.Fields(rel) {
		if strings.EqualFold(v, value) {
			return true
		}
	}
	return false
}

func resolveHref(base *url.URL, href string) (*url.URL, bool) {
	// From the HTML spec, remove leading/trailing whitespace,
	// as well as tabs/whitespace from the URL.
	href = strings.TrimSpace(href)
	href = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, href)

	// Skip links that point back to the same page
	if href == "" || strings.HasPrefix(href, "#") {
		return nil, false
	}

	ref, err := url.Parse(href)
	if err != nil {
		return nil, false
	}

	// Only want to resolve HTTP & HTTPS references.
	// Skip other schemes (e.g. mailto:, tel:, etc.)
	u := base.ResolveReference(ref)
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, false
	}

	// Fragment not important for HREFs
	u.Fragment = ""
	u.RawFragment = ""

	return u, true
}
