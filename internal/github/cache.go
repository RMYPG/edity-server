package github

import (
	"container/list"
	"sync"
)

// cacheBytes bounds what the cache holds: the answers about a few hundred
// Publishes, each a few tens of kilobytes.
const cacheBytes = 16 << 20

// maxCachedBody is the largest answer the cache keeps, so that one large
// file does not push out everything else.
const maxCachedBody = 1 << 20

// A cache keeps GitHub's latest answer to each GET call with its ETag, so
// that the call can ask GitHub whether it changed. GitHub answers 304 Not
// Modified when it did not, which costs nothing against the installation's
// rate limit, and the call returns the answer it kept. It forgets the
// answers used least recently first. It is safe for concurrent use.
type cache struct {
	limit int

	mu   sync.Mutex
	size int
	// order holds the *cached answers, the one used most recently first.
	order *list.List
	keys  map[string]*list.Element
}

type cached struct {
	key    string
	etag   string
	answer answer
}

func newCache(limit int) *cache {
	return &cache{limit: limit, order: list.New(), keys: map[string]*list.Element{}}
}

// cacheKey is what tells two GET calls apart: their path and query, and the
// media type they accept. The token is not part of it: GitHub tells whether
// an ETag still holds for the call's token.
func cacheKey(r request) string {
	return r.accept + " " + r.path
}

func (c *cache) get(key string) *cached {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.keys[key]
	if !ok {
		return nil
	}
	c.order.MoveToFront(e)
	return e.Value.(*cached)
}

func (c *cache) put(key, etag string, a answer) {
	if len(a.body) > maxCachedBody {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.keys[key]; ok {
		c.size -= len(e.Value.(*cached).answer.body)
		c.order.Remove(e)
	}
	c.keys[key] = c.order.PushFront(&cached{key: key, etag: etag, answer: a})
	c.size += len(a.body)
	for c.size > c.limit {
		oldest := c.order.Back()
		old := oldest.Value.(*cached)
		c.order.Remove(oldest)
		delete(c.keys, old.key)
		c.size -= len(old.answer.body)
	}
}
