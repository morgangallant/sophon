package sophon

import (
	"container/heap"
	"context"
	"errors"
	"net/url"
	"sync"
	"time"
)

// Represents a unit of work to be crawled.
type crawlItem struct {
	// URL to be crawled.
	URL *url.URL

	// The normalized form of the URL, e.g. for deduplication.
	normalizedURL string

	// The host this URL belongs to, this is used as the "politeness key".
	// TODO consider also using the IP address to avoid hammering multiple
	// hosts that are serviced by the same machine
	// TODO incorporate golang.org/x/net/publicsuffix to handle subdomain politeness
	host string

	// The number of hops from a seed URL.
	depth int

	// The page this URL was discovered on, nil for seeds.
	referrer *url.URL
}

// Individual state for a particular host, e.g. for scheduling, politeness, etc.
type hostState struct {
	name        string
	queue       []*crawlItem
	nextAllowed time.Time
	index       int // index in the ready heap, -1 if not in it

	// TODO consider allowing more than one concurrent request to
	// large hosts, e.g. to avoid serializing work for large sites
	busy bool // whether there's a current in-flight request to this host
}

// popNext removes and returns the next crawl item from the host's queue.
// Panics, intentionally, if the queue is empty.
func (h *hostState) popNext() *crawlItem {
	item := h.queue[0]
	h.queue[0] = nil
	h.queue = h.queue[1:]
	return item
}

// A helper type to order hosts by their next allowed time.
// It only contains hosts that have queued URLs and aren't busy.
type hostHeap []*hostState

func (h hostHeap) Len() int { return len(h) }

func (h hostHeap) Less(i, j int) bool {
	return h[i].nextAllowed.Before(h[j].nextAllowed)
}

func (h hostHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *hostHeap) Push(x any) {
	s := x.(*hostState)
	s.index = len(*h)
	*h = append(*h, s)
}

func (h *hostHeap) Pop() any {
	old := *h
	n := len(old)
	s := old[n-1]
	old[n-1] = nil
	s.index = -1
	*h = old[:n-1]
	return s
}

// frontier manages the crawl queue and host states.
//
// TODO currently, this is entirely in-memory, which is going to be an issue
// for large crawls. we'll want to persist / checkpoint the frontier state to disk.
type frontier struct {
	// TODO eventually may need to shard this mutex
	mu sync.Mutex

	// TODO this currently stores full URLs as strings. This will need
	// to be memory-optimized (e.g. some sort of 64-bit fingerprint), and
	// likely disk-backed.
	seen map[string]struct{}

	// TODO evict idle hosts with some sort of TTL?
	hosts map[string]*hostState

	// TODO together with the per-host queues, this heap forms the "back queues" of a
	// Mercator-style frontier (i.e. politeness, "who can I crawl right now"). Consider
	// adding priority front queues ahead of it ("which URLs matter most"), so higher
	// priority URLs are crawled first. This heap should stay ordered by nextAllowed,
	// priority belongs in the front queues, not here.
	ready hostHeap

	inFlight int
	wake     chan struct{}

	// The delay between a request to a host finishing (i.e. Done being called),
	// and the next request to that same host being allowed to start.
	//
	// TODO make this adaptive, e.g. scale with fetch time, respect robots.txt Crawl-delay,
	// and backoff on 429/5xx.
	hostDelay time.Duration

	// Maximum number of URLs to admit, 0 for unlimited.
	maxAdmitted int
	admitted    int

	// TODO consider per-domain budgets (e.g. max number of URLs per host),
	// and penalties for URL length / repeated path segments.
}

// newFrontier creates an empty frontier. hostDelay is the minimum time between requests
// to the same host, and maxAdmitted caps the number of URLs admitted (0 for unlimited).
func newFrontier(hostDelay time.Duration, maxAdmitted int) *frontier {
	return &frontier{
		seen:        make(map[string]struct{}),
		hosts:       make(map[string]*hostState),
		wake:        make(chan struct{}),
		hostDelay:   hostDelay,
		maxAdmitted: maxAdmitted,
	}
}

// Add a URL to the frontier, to be crawled at some point. If the URL is already seen,
// or the max number of admitted URLs has been reached, then it is not admitted and
// this function returns false.
func (f *frontier) Add(u, referrer *url.URL, depth int) bool {
	var (
		normalized = normalizeURL(u)
		normString = normalized.String()
	)

	f.mu.Lock()
	defer f.mu.Unlock()

	// If we've already seen this URL, then don't admit it.
	if _, ok := f.seen[normString]; ok {
		return false
	}

	// If we've reached the max number of admitted URLs, then don't admit it.
	if f.maxAdmitted > 0 && f.admitted >= f.maxAdmitted {
		return false
	}

	// Important to mark the URL as seen at enqueue time, not fetch time, e.g. so
	// a URL linked from many pages is only ever enqueued once.
	f.seen[normString] = struct{}{}
	f.admitted++

	item := &crawlItem{
		URL:           u,
		normalizedURL: normString,
		host:          normalized.Host,
		depth:         depth,
		referrer:      referrer,
	}

	h := f.hosts[normalized.Host]
	if h == nil {
		h = &hostState{
			name:  normalized.Host,
			index: -1,
			// nextAllowed is zero'ed, meaning it's eligible for crawling immediately.
		}
		f.hosts[normalized.Host] = h
	}
	h.queue = append(h.queue, item)

	// Add it to the ready heap, if not already busy / already in the heap.
	if !h.busy && h.index < 0 {
		heap.Push(&f.ready, h)
	}

	// If any Next() calls are waiting, wake them up.
	f.notifyLocked()

	return true
}

// Wakes up all blocked Next() calls. Requires the lock (f.mu) to be held.
func (f *frontier) notifyLocked() {
	close(f.wake)
	f.wake = make(chan struct{})
}

// Returned by Next once nothing is queued and nothing is in flight.
// Indicates the completion of the crawl, or at least, that we're unable
// to crawl any more links.
var errFrontierDone = errors.New("frontier exhausted")

// Next returns the next item to be crawled, blocking until one is available,
// either because every host is waiting out its hostDelay, or because nothing
// is queued and items are still in flight. Once the frontier is exhausted,
// it returns errFrontierDone.
//
// Caller _must_ call Done() on the returned item when processing is complete.
func (f *frontier) Next(ctx context.Context) (*crawlItem, error) {
	for {
		// Check the context early to avoid blocking if it's already cancelled.
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		f.mu.Lock()

		// How long until the earliest host becomes eligible, if any are queued.
		var wait time.Duration

		if len(f.ready) > 0 {
			h := f.ready[0]
			wait = time.Until(h.nextAllowed)

			// If the wait time is non-positive, the host is ready to be processed
			// immediately.
			if wait <= 0 {
				heap.Pop(&f.ready)

				// Pop the next item from the host's queue and mark it as busy.
				item := h.popNext()
				h.busy = true
				f.inFlight++

				f.mu.Unlock()
				return item, nil
			}
		} else if f.inFlight == 0 {
			f.mu.Unlock()
			return nil, errFrontierDone // Nothing left to crawl
		}

		// It's possible that wait = 0 here, e.g. we're not waiting for
		// a host to become eligible, but rather, in-flight requests to finish.
		var timerChan <-chan time.Time // a nil channel never fires
		if wait > 0 {
			timerChan = time.After(wait)
		}

		// Unlock the mutex, and wait for one of three conditions:
		// - The host becomes eligible
		// - The wait time expires
		// - The context is cancelled
		wake := f.wake
		f.mu.Unlock()
		select {
		case <-wake:
		case <-timerChan:
		case <-ctx.Done():
		}
	}
}

// Done marks an item returned by Next as processed, and must be called exactly once per
// item, whether processing succeeded or failed. Any links discovered while processing
// the item must be added _before_ calling Done, otherwise, Next might see an empty frontier
// and end the crawl early.
func (f *frontier) Done(item *crawlItem) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// We've completed a request to this host, so it's no longer busy,
	// and we must update its nextAllowed time to allow future requests.
	h := f.hosts[item.host]

	// Calling Done twice for the same item (or for an item that didn't come from Next)
	// would push the host onto the heap twice and drive inFlight negative, so fail loudly.
	if !h.busy {
		panic("frontier: Done called for host with no in-flight request: " + item.host)
	}

	h.busy = false
	h.nextAllowed = time.Now().Add(f.hostDelay)
	f.inFlight--

	// If the host has more work to be done, we'll re-add it to our ready heap.
	if len(h.queue) > 0 {
		heap.Push(&f.ready, h)
	}

	// A pending Next() call may be waiting, so we wake it up if necessary.
	f.notifyLocked()
}
