package sophon

import (
	"net"
	"net/url"
	"path"
	"strings"
)

// Canonicalize a URL, e.g. convert to lowercase, remove fragment, etc.
// This is used for deduplication of URLs.
func normalizeURL(u *url.URL) *url.URL {
	n := *u
	n.Scheme = strings.ToLower(n.Scheme) // "HTTP" -> "http", etc.

	// These aren't relevant for deduplication
	n.Fragment = ""
	n.RawFragment = ""
	n.User = nil
	n.ForceQuery = false

	var (
		host = strings.TrimSuffix(strings.ToLower(n.Hostname()), ".")
		port = n.Port()
	)

	// If the port is the default one for the scheme, we can drop it.
	if (n.Scheme == "http" && port == "80") || (n.Scheme == "https" && port == "443") {
		port = ""
	}

	if port != "" {
		n.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		n.Host = "[" + host + "]"
	} else {
		n.Host = host
	}

	// Resolve "." and ".." segments, use "/" for an empty path,
	// and keep any trailing slash.
	escaped := n.EscapedPath()
	if escaped == "" {
		escaped = "/"
	}
	cleaned := path.Clean(escaped) // Make sure an encoded slash ("%2F") isn't treated as a seperator
	if strings.HasSuffix(escaped, "/") && cleaned != "/" {
		cleaned += "/"
	}
	if p, err := url.PathUnescape(cleaned); err == nil {
		n.Path = p
		n.RawPath = cleaned
	}

	// Avoid dropping query patterns that don't parse cleanly (e.g. bad escapes).
	if q, err := url.ParseQuery(n.RawQuery); err == nil {
		n.RawQuery = q.Encode()
	}

	return &n
}
