package marketdata

import (
	"github.com/quickfixgo/quickfix"
	"github.com/shopspring/decimal"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/marketdataincrementalrefresh"
	"github.com/quynv/mock-fixser/fix/fix44/marketdatarequestreject"
	"github.com/quynv/mock-fixser/fix/fix44/marketdatasnapshotfullrefresh"
	"github.com/quynv/mock-fixser/fix/fix44/quotecancel"
)

// view selects what a subscription receives from a quote.
type view struct {
	depth      int  // levels per side; 0 = all
	bid, offer bool // requested MDEntryTypes
}

func (v view) levels(q Quote) int {
	n := len(q.Bids)
	if v.depth > 0 && v.depth < n {
		n = v.depth
	}
	return n
}

// entries is the number of NoMDEntries groups a quote produces.
func (v view) entries(q Quote) int {
	n := 0
	if v.bid {
		n += v.levels(q)
	}
	if v.offer {
		n += v.levels(q)
	}
	return n
}

func px(v float64, decimals int32) decimal.Decimal {
	return decimal.NewFromFloat(v).Round(decimals)
}

// snapshotMsg builds W for one symbol (W holds a single instrument).
func snapshotMsg(mdReqID string, q Quote, v view) *quickfix.Message {
	m := marketdatasnapshotfullrefresh.New()
	if mdReqID != "" {
		m.SetMDReqID(mdReqID)
	}
	m.SetSymbol(q.Symbol)
	g := marketdatasnapshotfullrefresh.NewNoMDEntriesRepeatingGroup()
	add := func(typ enum.MDEntryType, levels []Level) {
		for i := range v.levels(q) {
			e := g.Add()
			e.SetMDEntryType(typ)
			e.SetMDEntryPx(px(levels[i].Px, q.Decimals), q.Decimals)
			e.SetMDEntrySize(decimal.NewFromFloat(levels[i].Size), 0)
			e.SetMDEntryPositionNo(i + 1)
			if q.QuoteCondition != "" {
				e.SetQuoteCondition(enum.QuoteCondition(q.QuoteCondition))
			}
		}
	}
	if v.bid {
		add(enum.MDEntryType_BID, q.Bids)
	}
	if v.offer {
		add(enum.MDEntryType_OFFER, q.Asks)
	}
	m.SetNoMDEntries(g)
	return m.ToMessage()
}

// incItem is one quote for one subscription inside an X message.
type incItem struct {
	q      Quote
	v      view
	action enum.MDUpdateAction
}

// incrementalMsg builds X for one or more symbols. mdReqID is empty for
// per_session batches.
func incrementalMsg(mdReqID string, items []incItem) *quickfix.Message {
	m := marketdataincrementalrefresh.New()
	if mdReqID != "" {
		m.SetMDReqID(mdReqID)
	}
	g := marketdataincrementalrefresh.NewNoMDEntriesRepeatingGroup()
	for _, it := range items {
		add := func(typ enum.MDEntryType, levels []Level) {
			for i := range it.v.levels(it.q) {
				e := g.Add()
				e.SetMDUpdateAction(it.action)
				e.SetMDEntryType(typ)
				e.SetSymbol(it.q.Symbol)
				e.SetMDEntryPx(px(levels[i].Px, it.q.Decimals), it.q.Decimals)
				e.SetMDEntrySize(decimal.NewFromFloat(levels[i].Size), 0)
				e.SetMDEntryPositionNo(i + 1)
				if it.q.QuoteCondition != "" {
					e.SetQuoteCondition(enum.QuoteCondition(it.q.QuoteCondition))
				}
			}
		}
		if it.v.bid {
			add(enum.MDEntryType_BID, it.q.Bids)
		}
		if it.v.offer {
			add(enum.MDEntryType_OFFER, it.q.Asks)
		}
	}
	m.SetNoMDEntries(g)
	return m.ToMessage()
}

func rejectMsg(mdReqID string, reason enum.MDReqRejReason, text string) *quickfix.Message {
	m := marketdatarequestreject.New(field.NewMDReqID(mdReqID))
	m.SetMDReqRejReason(reason)
	if text != "" {
		m.SetText(text)
	}
	return m.ToMessage()
}

// quoteCancelMsg builds QuoteCancel (Z): type 4 (all quotes) without
// symbols, type 1 (listed securities) with them.
func quoteCancelMsg(quoteID string, symbols []string) *quickfix.Message {
	typ := enum.QuoteCancelType_CXLALL
	if len(symbols) > 0 {
		typ = enum.QuoteCancelType_CXLSYM
	}
	m := quotecancel.New(field.NewQuoteID(quoteID), field.NewQuoteCancelType(typ))
	if len(symbols) > 0 {
		g := quotecancel.NewNoQuoteEntriesRepeatingGroup()
		for _, s := range symbols {
			g.Add().SetSymbol(s)
		}
		m.SetNoQuoteEntries(g)
	}
	return m.ToMessage()
}
