package sophon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"time"
)

// Resource is a representation of a fetched resource, including its content and metadata.
type Resource struct {
	// Content holds the raw bytes of the fetched resource.
	Content []byte

	// The final URL of the fetched resource.
	// For example, if the resource was redirected, this will be the final URL.
	URL string

	// If the response was truncated, e.g. was over a size limit,
	// then this will be true so downstream callers know Content is incomplete.
	Truncated bool

	ContentType  string
	ETag         string
	LastModified time.Time

	// Statistics about the fetch itself, e.g. performance of the site.
	FetchedAt     time.Time
	FetchDuration time.Duration
}

// A Fetcher can retrieve a resource from a given URL.
type Fetcher interface {
	// Fetch retrieves the resource at the given URL.
	// It returns the fetched Resource and any error encountered.
	Fetch(ctx context.Context, url string) (*Resource, error)
}

// TODO include the URL and Retry-After header, so callers can log which URL failed and back off
// hosts that return 429/503.
type HTTPFetchError struct {
	StatusCode int
}

func (e *HTTPFetchError) Error() string {
	return fmt.Sprintf("http fetch error: status code %d", e.StatusCode)
}

// DefaultFetcher uses net/http and a default http.Client, along with some sane
// defaults. It's recommended for most users.
type DefaultFetcher struct {
	client          *http.Client
	userAgent       string
	timeout         time.Duration
	maxResponseSize int64
}

const (
	// Maximum number of redirects to follow before giving up.
	defaultMaxRedirects = 5

	// Per-phase timeouts, these let us bail on dead or stalled hosts well before the overall
	// request timeout (which mostly exists to bound slow body reads).
	defaultDialTimeout           = 10 * time.Second
	defaultTLSHandshakeTimeout   = 10 * time.Second
	defaultResponseHeaderTimeout = 15 * time.Second

	// Overall request timeout, including reading the response body.
	defaultRequestTimeout = 30 * time.Second

	defaultMaxResponseSize = 4 << 20 // 4 MiB

	// Number of bytes http.DetectContentType looks at when sniffing a content type.
	sniffLen = 512
)

// TODO allow more configuration here
func NewDefaultFetcher(userAgent string) *DefaultFetcher {
	// Use sane defaults of http.DefaultTransport, adding in timeouts.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   defaultDialTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	transport.TLSHandshakeTimeout = defaultTLSHandshakeTimeout
	transport.ResponseHeaderTimeout = defaultResponseHeaderTimeout

	// This approach to redirect handling here (a) bypasses robots.txt and (b) per-host politeness
	// for every hop after the first. We should eventually return http.ErrUseLastResponse and surface
	// 3xx responses (status + Location) to the caller, so it can schedule the fetch for the new URL.
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= defaultMaxRedirects {
				return fmt.Errorf("stopped after %d redirects", len(via))
			}
			return nil
		},
	}

	return &DefaultFetcher{
		client:          client,
		userAgent:       userAgent,
		timeout:         defaultRequestTimeout,
		maxResponseSize: defaultMaxResponseSize,
	}
}

func (f *DefaultFetcher) Fetch(ctx context.Context, rawURL string) (*Resource, error) {
	res := &Resource{URL: rawURL}

	if f.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", f.userAgent)

	// The logic here is that:
	// - We prefer HTML over all other content types
	// - XHTML is fine, but lower priority than HTML
	// - If not HTML/XHTML, fine, just accept whatever
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.1")

	res.FetchedAt = time.Now()

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch resource: %w", err)
	}
	defer resp.Body.Close()

	// Set the final URL, in case it was redirected
	res.URL = resp.Request.URL.String()

	// If we get a non-200 status code, we'll do a best-effort drain of the response body
	// (something small) in an attempt to re-use the connection.
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, &HTTPFetchError{resp.StatusCode}
	}

	res.ContentType = resp.Header.Get("Content-Type")
	res.ETag = resp.Header.Get("ETag")
	if lm, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		res.LastModified = lm
	}

	// Buffer the body so we can peek at the first bytes (to sniff the content type) without
	// consuming them.
	body := bufio.NewReaderSize(resp.Body, sniffLen)

	// If the server didn't tell us the content type, sniff it from the first bytes of the body.
	// We do this before reading the full body, so we don't download e.g. a large PDF only to
	// throw it away.
	if res.ContentType == "" {
		// Peek returns io.EOF if the body is shorter than sniffLen, which is fine, we just sniff
		// whatever we got.
		head, err := body.Peek(sniffLen)
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("failed to read response body: %w", err)
		}
		res.ContentType = http.DetectContentType(head)
	}

	mediaType, _, _ := mime.ParseMediaType(res.ContentType)
	if !isHTMLMediaType(mediaType) {
		// TODO this should be a typed error, so callers can handle it properly
		return nil, fmt.Errorf("unsupported media type: %s", res.ContentType)
	}

	// Set a limit on the response body size
	// +1 to be able to detect truncated responses
	var bodyReader io.Reader = body
	if f.maxResponseSize > 0 {
		bodyReader = io.LimitReader(body, f.maxResponseSize+1)
	}

	res.Content, err = io.ReadAll(bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Fetch duration includes the time taken to read the response body
	res.FetchDuration = time.Since(res.FetchedAt)

	if f.maxResponseSize > 0 {
		// If we got the extra byte, it means the response body was truncated
		res.Truncated = int64(len(res.Content)) > f.maxResponseSize
		if res.Truncated {
			res.Content = res.Content[:f.maxResponseSize]
		}
	}

	return res, nil
}

// isHTMLMediaType reports whether mediaType (without parameters, e.g. "text/html") is a type we
// know how to parse.
func isHTMLMediaType(mediaType string) bool {
	switch mediaType {
	case "text/html", "application/xhtml+xml":
		return true
	default:
		return false
	}
}
