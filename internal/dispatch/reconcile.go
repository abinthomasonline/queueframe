package dispatch

import (
	"context"
	"errors"
	"math/big"
	"strings"

	"github.com/abinthomasonline/queueframe/internal/broker"
	"github.com/abinthomasonline/queueframe/internal/domain"
)

var (
	ErrNoMatch         = errors.New("no matching Kite order")
	ErrMultipleMatches = errors.New("multiple matching Kite orders")
)

func MatchOrders(job PreparedJob, observedAccountID int64, observed []broker.Order) []broker.Order {
	if observedAccountID != job.AccountID {
		return nil
	}
	var matches []broker.Order
	for _, order := range observed {
		if order.Tag == job.Tag && paramsEqual(job.Params, order.Params) {
			matches = append(matches, order)
		}
	}
	return matches
}

func Reconcile(ctx context.Context, job PreparedJob) (broker.Order, error) {
	orders, err := job.Client.Orders(ctx)
	if err != nil {
		return broker.Order{}, err
	}
	matches := MatchOrders(job, job.AccountID, orders)
	switch len(matches) {
	case 0:
		return broker.Order{}, ErrNoMatch
	case 1:
		return matches[0], nil
	default:
		return broker.Order{}, ErrMultipleMatches
	}
}

func paramsEqual(want, got domain.OrderParams) bool {
	if !strings.EqualFold(want.Exchange, got.Exchange) ||
		!strings.EqualFold(want.TradingSymbol, got.TradingSymbol) ||
		!strings.EqualFold(want.TransactionType, got.TransactionType) ||
		!strings.EqualFold(want.Product, got.Product) ||
		!strings.EqualFold(want.OrderType, got.OrderType) ||
		!strings.EqualFold(want.Validity, got.Validity) ||
		want.Quantity != got.Quantity || want.DisclosedQuantity != got.DisclosedQuantity ||
		want.ValidityTTL != got.ValidityTTL {
		return false
	}
	if !decimalEqual(want.Price, got.Price) || !decimalEqual(want.TriggerPrice, got.TriggerPrice) ||
		!decimalEqual(want.MarketProtection, got.MarketProtection) {
		return false
	}
	// Kite's order book currently does not expose whether autoslice was set.
	if got.Autoslice && !want.Autoslice {
		return false
	}
	return true
}

func decimalEqual(left, right string) bool {
	if strings.TrimSpace(left) == "" {
		left = "0"
	}
	if strings.TrimSpace(right) == "" {
		right = "0"
	}
	a, ok := new(big.Rat).SetString(left)
	if !ok {
		return false
	}
	b, ok := new(big.Rat).SetString(right)
	return ok && a.Cmp(b) == 0
}
