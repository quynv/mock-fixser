package fixserver

import (
	"sync"
	"time"

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
	base, err := c.base.Create(id)
	if err != nil {
		return nil, err
	}
	st := &lockedStore{s: base}
	c.stores[id] = st
	return st, nil
}

// drop forgets a session's store so a re-added session starts at seq 1.
func (c *storeCache) drop(id quickfix.SessionID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.stores, id)
}

// lockedStore serializes access to a MessageStore. quickfixgo's MemoryStore
// is not safe for concurrent use, and quickfix.GetExpectedSenderNum (used
// by the admin API) reads it from outside the session goroutine.
type lockedStore struct {
	mu sync.Mutex
	s  quickfix.MessageStore
}

func (l *lockedStore) NextSenderMsgSeqNum() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.s.NextSenderMsgSeqNum()
}

func (l *lockedStore) NextTargetMsgSeqNum() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.s.NextTargetMsgSeqNum()
}

func (l *lockedStore) IncrNextSenderMsgSeqNum() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.s.IncrNextSenderMsgSeqNum()
}

func (l *lockedStore) IncrNextTargetMsgSeqNum() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.s.IncrNextTargetMsgSeqNum()
}

func (l *lockedStore) SetNextSenderMsgSeqNum(next int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.s.SetNextSenderMsgSeqNum(next)
}

func (l *lockedStore) SetNextTargetMsgSeqNum(next int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.s.SetNextTargetMsgSeqNum(next)
}

func (l *lockedStore) CreationTime() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.s.CreationTime()
}

func (l *lockedStore) SetCreationTime(t time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.s.SetCreationTime(t)
}

func (l *lockedStore) SaveMessage(seqNum int, msg []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.s.SaveMessage(seqNum, msg)
}

func (l *lockedStore) SaveMessageAndIncrNextSenderMsgSeqNum(seqNum int, msg []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.s.SaveMessageAndIncrNextSenderMsgSeqNum(seqNum, msg)
}

func (l *lockedStore) GetMessages(begin, end int) ([][]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.s.GetMessages(begin, end)
}

func (l *lockedStore) IterateMessages(begin, end int, cb func([]byte) error) error {
	msgs, err := l.GetMessages(begin, end)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if err := cb(m); err != nil {
			return err
		}
	}
	return nil
}

func (l *lockedStore) Refresh() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.s.Refresh()
}

func (l *lockedStore) Reset() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.s.Reset()
}

func (l *lockedStore) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.s.Close()
}
