package order

import (
	"fmt"
	"strings"
	"time"

	"github.com/quickfixgo/quickfix"
	"github.com/shopspring/decimal"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/businessmessagereject"
	"github.com/quynv/mock-fixser/fix/fix44/executionreport"
	"github.com/quynv/mock-fixser/fix/fix44/ordercancelreject"
)

// erFields are the per-report parts of an ExecutionReport.
type erFields struct {
	execID       string
	execType     string
	ordRejReason string
	text         string
	lastQty      *decimal.Decimal
	lastPx       *decimal.Decimal
	now          time.Time
}

// scale is the number of decimals needed to print d without loss.
func scale(d decimal.Decimal) int32 {
	s := d.String()
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return int32(len(s) - i - 1)
	}
	return 0
}

func dec(d decimal.Decimal) (decimal.Decimal, int32) { return d, scale(d) }

func executionReport(o *Order, f erFields) *quickfix.Message {
	avgPx := o.AvgPx.Round(8)
	m := executionreport.New(
		field.NewOrderID(o.OrderID),
		field.NewExecID(f.execID),
		field.NewExecType(enum.ExecType(f.execType)),
		field.NewOrdStatus(o.Status),
		field.NewSide(enum.Side(o.Side)),
		field.NewLeavesQty(dec(o.LeavesQty)),
		field.NewCumQty(dec(o.CumQty)),
		field.NewAvgPx(dec(avgPx)),
	)
	m.SetClOrdID(o.ClOrdID)
	if o.OrigClOrdID != "" {
		m.SetOrigClOrdID(o.OrigClOrdID)
	}
	m.SetSymbol(o.Symbol)
	if !o.OrderQty.IsZero() {
		m.SetOrderQty(dec(o.OrderQty))
	}
	if o.OrdType != "" {
		m.SetOrdType(enum.OrdType(o.OrdType))
	}
	if o.Account != "" {
		m.SetAccount(o.Account)
	}
	if f.lastQty != nil {
		m.SetLastQty(dec(*f.lastQty))
		m.SetLastPx(dec(*f.lastPx))
	}
	if f.ordRejReason != "" {
		m.SetOrdRejReason(enum.OrdRejReason(f.ordRejReason))
	}
	if f.text != "" {
		m.SetText(f.text)
	}
	m.SetTransactTime(f.now)
	return m.ToMessage()
}

func cancelReject(orderID, clOrdID, orig string, status enum.OrdStatus, to enum.CxlRejResponseTo,
	reason enum.CxlRejReason, text string, now time.Time) *quickfix.Message {
	m := ordercancelreject.New(field.NewOrderID(orderID), field.NewClOrdID(clOrdID), field.NewOrigClOrdID(orig),
		field.NewOrdStatus(status), field.NewCxlRejResponseTo(to))
	m.SetCxlRejReason(reason)
	if text != "" {
		m.SetText(text)
	}
	m.SetTransactTime(now)
	return m.ToMessage()
}

func businessReject(refMsgType, refID string, reason int, text string) *quickfix.Message {
	m := businessmessagereject.New(field.NewRefMsgType(refMsgType),
		field.NewBusinessRejectReason(enum.BusinessRejectReason(fmt.Sprint(reason))))
	m.SetBusinessRejectRefID(refID)
	if text != "" {
		m.SetText(text)
	}
	return m.ToMessage()
}
