package fixserver

import (
	"sync"

	"github.com/quickfixgo/quickfix"
)

// storeCache keeps one MessageStore per session. quickfixgo creates a new
// session object for every dynamic connection; reusing the store keeps
// sequence numbers across reconnects within one run (section 5.1).
type storeCache struct {
	base quickfix.MessageStoreFactory

	mu     sync.Mutex
	stores map[quickfix.SessionID]quickfix.MessageStore
}

func newStoreCache(base quickfix.MessageStoreFactory) *storeCache {
	return &storeCache{base: base, stores: map[quickfix.SessionID]quickfix.MessageStore{}}
}

func (c *storeCache) Create(id quickfix.SessionID) (quickfix.MessageStore, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st, ok := c.stores[id]; ok {
		return st, nil
	}
	st, err := c.base.Create(id)
	if err != nil {
		return nil, err
	}
	c.stores[id] = st
	return st, nil
}

// drop forgets a session's store so a re-added session starts at seq 1.
func (c *storeCache) drop(id quickfix.SessionID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.stores, id)
}
