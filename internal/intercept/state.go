package intercept

import (
	"container/list"
	"hash/maphash"
	"sync"
	"time"

	"github.com/MelonSmasher/cliproxy-costs/internal/pricing"
	"github.com/MelonSmasher/cliproxy-costs/internal/usagebody"
)

// stream is the per-stream state kept between chunks (schema 6 payload
// chunks carry neither the request body nor history).
type stream struct {
	mu      sync.Mutex // serializes mutable usage state for concurrent callbacks
	done    bool
	format  string
	card    *pricing.Card // nil when unknown
	inject  bool
	start   usagebody.MessagesUsage
	expires time.Time
	key     string
	elem    *list.Element
}

const shards = 16

type shard struct {
	mu    sync.Mutex
	items map[string]*stream
	order *list.List // front = most recent
}

// States is a sharded LRU with TTL, keyed by interceptor RequestID.
type States struct {
	seed   maphash.Seed
	shards [shards]shard
	cap    int // per shard
	ttl    time.Duration
	now    func() time.Time
}

// NewStates creates a store holding at most maxEntries streams.
func NewStates(maxEntries int, ttl time.Duration) *States {
	s := &States{seed: maphash.MakeSeed(), cap: max(1, maxEntries/shards), ttl: ttl, now: time.Now}
	for i := range s.shards {
		s.shards[i].items = map[string]*stream{}
		s.shards[i].order = list.New()
	}
	return s
}

func (s *States) shard(key string) *shard {
	return &s.shards[maphash.String(s.seed, key)%shards]
}

// Put stores state for key, evicting the least recently used entry of the
// shard when full and expired entries opportunistically.
func (s *States) Put(key string, st *stream) {
	sh := s.shard(key)
	now := s.now()
	st.key, st.expires = key, now.Add(s.ttl)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if old, ok := sh.items[key]; ok {
		sh.order.Remove(old.elem)
		delete(sh.items, key)
	}
	for e := sh.order.Back(); e != nil; {
		v := e.Value.(*stream)
		if len(sh.items) < s.cap && now.Before(v.expires) {
			break
		}
		prev := e.Prev()
		sh.order.Remove(e)
		delete(sh.items, v.key)
		e = prev
	}
	st.elem = sh.order.PushFront(st)
	sh.items[key] = st
}

// Get returns live state for key (refreshing its recency); expired → nil.
func (s *States) Get(key string) *stream {
	sh := s.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	st, ok := sh.items[key]
	if !ok {
		return nil
	}
	if !s.now().Before(st.expires) {
		sh.order.Remove(st.elem)
		delete(sh.items, key)
		return nil
	}
	sh.order.MoveToFront(st.elem)
	return st
}

// Delete drops state for key.
func (s *States) Delete(key string) {
	sh := s.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if st, ok := sh.items[key]; ok {
		sh.order.Remove(st.elem)
		delete(sh.items, key)
	}
}

// Len returns the number of stored entries (including not yet evicted expired ones).
func (s *States) Len() int {
	n := 0
	for i := range s.shards {
		s.shards[i].mu.Lock()
		n += len(s.shards[i].items)
		s.shards[i].mu.Unlock()
	}
	return n
}

// deleteIf avoids a late callback deleting a replacement stream with the
// same request id after a repeated header-init.
func (s *States) deleteIf(key string, expected *stream) {
	sh := s.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.items[key] == expected {
		sh.order.Remove(expected.elem)
		delete(sh.items, key)
	}
}
