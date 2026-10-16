package marketdata

import (
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/quickfixgo/quickfix"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/fix44/marketdatarequest"
	"github.com/quynv/mock-fixser/internal/clock"
	"github.com/quynv/mock-fixser/internal/config"
	"github.com/quynv/mock-fixser/internal/fixver"
	"github.com/quynv/mock-fixser/internal/metrics"
)

// QuoteSource is the part of the Engine the publisher needs.
type QuoteSource interface {
	Last(symbol string) (Quote, config.Symbol, bool)
}

// subscription is one active V (SubscriptionRequestType=1) of a session.
type subscription struct {
	mdReqID string
	symbols []string
	v       view
	full    bool // MDUpdateType=0: stream W instead of X
	w       *writer
	active  atomic.Bool
}

type sessionState struct {
	subs map[string]*subscription // by MDReqID
	w    *writer
}

// Publisher answers V and fans quotes out to subscribed sessions
// (sections 6.3, 6.4).
type Publisher struct {
	md    QuoteSource
	clock clock.Clock
	log   *slog.Logger
	send  Sender

	batching atomic.Pointer[batchingSettings]

	mu             sync.Mutex
	sessions       map[quickfix.SessionID]*sessionState
	bySymbol       map[string]map[*subscription]struct{}
	quoteCancelSeq int
}

type batchingSettings struct {
	global   config.Batching
	sessions map[config.SessionKey]config.Batching
	// bothSides: send bid and offer whatever the request asks for.
	bothSides        bool
	sessionBothSides map[config.SessionKey]bool
	monitor          config.SessionKey // never sent QuoteCancel
}

func NewPublisher(md QuoteSource, cfg *config.Config, clk clock.Clock, log *slog.Logger, send Sender) *Publisher {
	if send == nil {
		send = func(m *quickfix.Message, id quickfix.SessionID) error { return quickfix.SendToTarget(m, id) }
	}
	p := &Publisher{
		md: md, clock: clk, log: log, send: send,
		sessions: map[quickfix.SessionID]*sessionState{},
		bySymbol: map[string]map[*subscription]struct{}{},
	}
	p.SetConfig(cfg)
	return p
}

// SetConfig updates the batching settings; writers pick them up on their
// next send.
func (p *Publisher) SetConfig(cfg *config.Config) {
	s := &batchingSettings{
		global: cfg.MarketData.Batching, sessions: map[config.SessionKey]config.Batching{},
		bothSides: cfg.MarketData.SendBothSides, sessionBothSides: map[config.SessionKey]bool{},
		monitor: config.SessionKey{SenderCompID: cfg.Server.Monitor.SenderCompID, TargetCompID: cfg.Server.Monitor.TargetCompID},
	}
	for _, cs := range cfg.Server.FIX.Sessions {
		if cs.MarketData != nil && cs.MarketData.Batching != nil {
			s.sessions[cs.Key()] = mergeBatching(s.global, *cs.MarketData.Batching)
		}
		if cs.MarketData != nil && cs.MarketData.SendBothSides != nil {
			s.sessionBothSides[cs.Key()] = *cs.MarketData.SendBothSides
		}
	}
	// The UI gateway subscribes per symbol and is never batched, so the
	// chart does not depend on other clients' settings (section 6.6).
	mon := cfg.Server.Monitor
	noBatch := s.global
	noBatch.Enabled = false
	s.sessions[config.SessionKey{SenderCompID: mon.SenderCompID, TargetCompID: mon.TargetCompID}] = noBatch
	p.batching.Store(s)
}

func mergeBatching(b config.Batching, o config.BatchingOverride) config.Batching {
	if o.Enabled != nil {
		b.Enabled = *o.Enabled
	}
	if o.Scope != nil {
		b.Scope = *o.Scope
	}
	if o.FlushIntervalMs != nil {
		b.FlushIntervalMs = *o.FlushIntervalMs
	}
	if o.MaxSymbolsPerMsg != nil {
		b.MaxSymbolsPerMsg = *o.MaxSymbolsPerMsg
	}
	if o.MaxEntriesPerMsg != nil {
		b.MaxEntriesPerMsg = *o.MaxEntriesPerMsg
	}
	if o.Conflate != nil {
		b.Conflate = *o.Conflate
	}
	if o.InitialSnapshot != nil {
		b.InitialSnapshot = *o.InitialSnapshot
	}
	return b
}

func (p *Publisher) batchingFor(id quickfix.SessionID) config.Batching {
	s := p.batching.Load()
	if b, ok := s.sessions[config.SessionKey{SenderCompID: id.SenderCompID, TargetCompID: id.TargetCompID}]; ok {
		return b
	}
	return s.global
}

func (p *Publisher) bothSidesFor(id quickfix.SessionID) bool {
	s := p.batching.Load()
	if b, ok := s.sessionBothSides[config.SessionKey{SenderCompID: id.SenderCompID, TargetCompID: id.TargetCompID}]; ok {
		return b
	}
	return s.bothSides
}

// Conflated returns the number of ticks dropped for a session because a
// newer tick replaced them before they were sent.
func (p *Publisher) Conflated(id quickfix.SessionID) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sessions[id]; ok {
		return s.w.conflated.Load()
	}
	return 0
}

// OnQuote queues a quote for every subscription to its symbol. It never
// blocks on a session.
func (p *Publisher) OnQuote(q Quote) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for sub := range p.bySymbol[q.Symbol] {
		sub.w.enqueueTick(pendingTick{sub: sub, q: q, action: enum.MDUpdateAction_CHANGE})
	}
}

// OnLogout cancels every subscription of a session (section 6.3).
func (p *Publisher) OnLogout(id quickfix.SessionID) {
	p.mu.Lock()
	s, ok := p.sessions[id]
	if ok {
		for _, sub := range s.subs {
			p.unregisterLocked(sub)
		}
		delete(p.sessions, id)
	}
	p.mu.Unlock()
	if ok {
		s.w.close()
	}
}

// Close stops every session writer.
func (p *Publisher) Close() {
	p.mu.Lock()
	sessions := p.sessions
	p.sessions = map[quickfix.SessionID]*sessionState{}
	clear(p.bySymbol)
	p.mu.Unlock()
	for _, s := range sessions {
		s.w.close()
	}
}

// Route registers the V handler on a router, for every FIX version: the
// FIX 4.4 reader works for 4.2 and 5.0 requests, whose fields are a
// subset or share the same tags.
func (p *Publisher) Route(r *quickfix.MessageRouter) {
	route := func(m *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
		return p.HandleRequest(marketdatarequest.FromMessage(m), id)
	}
	for _, v := range fixver.RouteKeys {
		r.AddRoute(v, "V", route)
	}
}

// HandleRequest processes MarketDataRequest (V).
func (p *Publisher) HandleRequest(m marketdatarequest.MarketDataRequest, id quickfix.SessionID) quickfix.MessageRejectError {
	mdReqID, err := m.GetMDReqID()
	if err != nil {
		return err
	}
	typ, err := m.GetSubscriptionRequestType()
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessionLocked(id)

	if typ == enum.SubscriptionRequestType_UNSUBSCRIBE {
		if sub, ok := s.subs[mdReqID]; ok {
			p.unregisterLocked(sub)
			delete(s.subs, mdReqID)
		}
		return nil // no response (section 6.3)
	}

	reject := func(reason enum.MDReqRejReason, text string) quickfix.MessageRejectError {
		s.w.enqueueMsg(rejectMsg(mdReqID, reason, text))
		return nil
	}
	if typ != enum.SubscriptionRequestType_SNAPSHOT && typ != enum.SubscriptionRequestType_SNAPSHOTUPDATE {
		return reject(enum.MDReqRejReason_UNSUPPSUB, "")
	}

	v, rerr := requestView(m)
	if rerr != nil {
		return rerr
	}
	if p.bothSidesFor(id) {
		v.bid, v.offer = true, true
	}
	if !v.bid && !v.offer {
		return reject(enum.MDReqRejReason_UNSUPPENTRY, "only MDEntryType 0 (bid) and 1 (offer) are supported")
	}

	symbols, rerr := requestSymbols(m)
	if rerr != nil {
		return rerr
	}
	quotes := make([]Quote, len(symbols))
	for i, sym := range symbols {
		q, _, ok := p.md.Last(sym)
		if !ok {
			// The whole request is rejected and nothing is registered.
			return reject(enum.MDReqRejReason_UNKNOWNSYM, fmt.Sprintf("unknown symbol %s", sym))
		}
		quotes[i] = q
	}

	if typ == enum.SubscriptionRequestType_SNAPSHOT {
		for _, q := range quotes {
			s.w.enqueueMsg(snapshotMsg(mdReqID, q, v))
		}
		return nil
	}

	if _, dup := s.subs[mdReqID]; dup {
		return reject(enum.MDReqRejReason_DUPID, "")
	}
	sub := &subscription{mdReqID: mdReqID, symbols: symbols, v: v, w: s.w}
	if ut, err := m.GetMDUpdateType(); err == nil && ut == enum.MDUpdateType_FULL {
		sub.full = true
	}
	sub.active.Store(true)
	s.subs[mdReqID] = sub

	// Initial state: W per symbol, or one batched X with MDUpdateAction=New.
	b := p.batchingFor(id)
	if b.Enabled && b.InitialSnapshot == "batched_x" && !sub.full {
		for _, q := range quotes {
			s.w.enqueueTick(pendingTick{sub: sub, q: q, action: enum.MDUpdateAction_NEW})
		}
	} else {
		for _, q := range quotes {
			s.w.enqueueMsg(snapshotMsg(mdReqID, q, v))
		}
	}
	// Register after queueing the snapshot so the first X follows it.
	for _, sym := range symbols {
		if p.bySymbol[sym] == nil {
			p.bySymbol[sym] = map[*subscription]struct{}{}
		}
		p.bySymbol[sym][sub] = struct{}{}
		metrics.MDSubscriptions.WithLabelValues(sym).Inc()
	}
	return nil
}

func (p *Publisher) sessionLocked(id quickfix.SessionID) *sessionState {
	s, ok := p.sessions[id]
	if !ok {
		s = &sessionState{
			subs: map[string]*subscription{},
			w:    newWriter(id, p.send, p.clock, p.log, func() config.Batching { return p.batchingFor(id) }),
		}
		p.sessions[id] = s
	}
	return s
}

func (p *Publisher) unregisterLocked(sub *subscription) {
	if !sub.active.Swap(false) {
		return
	}
	for _, sym := range sub.symbols {
		metrics.MDSubscriptions.WithLabelValues(sym).Dec()
		delete(p.bySymbol[sym], sub)
		if len(p.bySymbol[sym]) == 0 {
			delete(p.bySymbol, sym)
		}
	}
}

// requestView reads MarketDepth and MDEntryTypes. MarketDepth=1 is top of
// book; any other value means up to depth_levels, capped at N when N > 1.
func requestView(m marketdatarequest.MarketDataRequest) (view, quickfix.MessageRejectError) {
	depth, err := m.GetMarketDepth()
	if err != nil {
		return view{}, err
	}
	v := view{depth: max(depth, 0)}
	types, err := m.GetNoMDEntryTypes()
	if err != nil {
		return view{}, err
	}
	for i := range types.Len() {
		t, err := types.Get(i).GetMDEntryType()
		if err != nil {
			return view{}, err
		}
		switch t {
		case enum.MDEntryType_BID:
			v.bid = true
		case enum.MDEntryType_OFFER:
			v.offer = true
		}
	}
	return v, nil
}

func requestSymbols(m marketdatarequest.MarketDataRequest) ([]string, quickfix.MessageRejectError) {
	group, err := m.GetNoRelatedSym()
	if err != nil {
		return nil, err
	}
	symbols := make([]string, 0, group.Len())
	seen := map[string]bool{}
	for i := range group.Len() {
		sym, err := group.Get(i).GetSymbol()
		if err != nil {
			return nil, err
		}
		if !seen[sym] {
			seen[sym] = true
			symbols = append(symbols, sym)
		}
	}
	return symbols, nil
}

// QuoteCancelResult reports what a QuoteCancel did.
type QuoteCancelResult struct {
	Sessions      []string `json:"sessions"`
	Subscriptions int      `json:"subscriptions_canceled"`
}

// QuoteCancel simulates the LP withdrawing quotes (runtime command): it
// drops the subscriptions to symbols (all symbols when empty) and sends
// each affected session a QuoteCancel (Z) with QuoteCancelType 1 (listed
// securities) or 4 (all quotes). The client has to subscribe again to get
// prices. only, when set, limits it to one session; the monitor session
// is never included.
func (p *Publisher) QuoteCancel(symbols []string, only *quickfix.SessionID) QuoteCancelResult {
	cancelAll := len(symbols) == 0
	want := map[string]bool{}
	for _, s := range symbols {
		want[s] = true
	}
	monitor := p.batching.Load().monitor
	res := QuoteCancelResult{Sessions: []string{}}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.quoteCancelSeq++
	for id, s := range p.sessions {
		if only != nil && id != *only {
			continue
		}
		if (config.SessionKey{SenderCompID: id.SenderCompID, TargetCompID: id.TargetCompID}) == monitor {
			continue
		}
		canceled := map[string]bool{}
		for reqID, sub := range s.subs {
			var keep []string
			for _, sym := range sub.symbols {
				if cancelAll || want[sym] {
					delete(p.bySymbol[sym], sub)
					if len(p.bySymbol[sym]) == 0 {
						delete(p.bySymbol, sym)
					}
					metrics.MDSubscriptions.WithLabelValues(sym).Dec()
					canceled[sym] = true
				} else {
					keep = append(keep, sym)
				}
			}
			if len(keep) == 0 {
				sub.active.Store(false)
				delete(s.subs, reqID)
				res.Subscriptions++
			} else if len(keep) != len(sub.symbols) {
				sub.symbols = keep
			}
		}
		if len(canceled) == 0 {
			continue
		}
		var syms []string
		if !cancelAll {
			for sym := range canceled {
				syms = append(syms, sym)
			}
			slices.Sort(syms)
		}
		s.w.enqueueMsg(quoteCancelMsg(fmt.Sprintf("QC-%d", p.quoteCancelSeq), syms))
		res.Sessions = append(res.Sessions, id.String())
	}
	slices.Sort(res.Sessions)
	return res
}
