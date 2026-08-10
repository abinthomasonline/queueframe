package orders

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/abinthomasonline/queueframe/internal/broker"
	"github.com/abinthomasonline/queueframe/internal/domain"
)

// Catalog is an in-memory snapshot of Zerodha's instrument master. It contains
// no quotes, prices, balances, or holdings.
type Catalog struct {
	mu       sync.RWMutex
	loadedAt time.Time
	symbols  map[string]domain.Instrument
	search   []domain.Instrument
}

func NewCatalog() *Catalog { return &Catalog{symbols: make(map[string]domain.Instrument)} }

func (c *Catalog) Refresh(ctx context.Context, client broker.Client) error {
	type result struct {
		instruments []domain.Instrument
		err         error
	}
	results := make(chan result, 2)
	for _, exchange := range []string{"NSE", "BSE"} {
		exchange := exchange
		go func() {
			instruments, err := client.Instruments(ctx, exchange)
			results <- result{instruments: instruments, err: err}
		}()
	}
	next := make(map[string]domain.Instrument)
	for range 2 {
		result := <-results
		if result.err != nil {
			return result.err
		}
		for _, instrument := range result.instruments {
			if !strings.EqualFold(instrument.InstrumentType, "EQ") {
				continue
			}
			instrument.Exchange = strings.ToUpper(strings.TrimSpace(instrument.Exchange))
			instrument.TradingSymbol = strings.ToUpper(strings.TrimSpace(instrument.TradingSymbol))
			instrument.Name = strings.TrimSpace(instrument.Name)
			next[key(instrument.Exchange, instrument.TradingSymbol)] = instrument
		}
	}
	if len(next) == 0 {
		return errors.New("instrument master contained no NSE/BSE equity symbols")
	}
	search := make([]domain.Instrument, 0, len(next))
	for _, instrument := range next {
		search = append(search, instrument)
	}
	sort.Slice(search, func(i, j int) bool {
		if search[i].TradingSymbol == search[j].TradingSymbol {
			return search[i].Exchange < search[j].Exchange
		}
		return search[i].TradingSymbol < search[j].TradingSymbol
	})
	c.mu.Lock()
	c.symbols = next
	c.search = search
	c.loadedAt = time.Now()
	c.mu.Unlock()
	return nil
}

// Search returns an in-memory, relevance-ranked equity-symbol result set. It
// performs no broker, database, quote, holding, or price lookup. Exact and
// prefix symbol matches rank ahead of instrument-name matches.
func (c *Catalog) Search(query string, limit int) []domain.Instrument {
	query = strings.ToUpper(strings.TrimSpace(query))
	if query == "" {
		return nil
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	type match struct {
		instrument domain.Instrument
		rank       int
	}
	c.mu.RLock()
	matches := make([]match, 0, limit)
	for _, instrument := range c.search {
		symbol := strings.ToUpper(instrument.TradingSymbol)
		name := strings.ToUpper(instrument.Name)
		rank := -1
		switch {
		case symbol == query:
			rank = 0
		case strings.HasPrefix(symbol, query):
			rank = 1
		case strings.HasPrefix(name, query):
			rank = 2
		case strings.Contains(symbol, query):
			rank = 3
		case strings.Contains(name, query):
			rank = 4
		}
		if rank >= 0 {
			matches = append(matches, match{instrument: instrument, rank: rank})
		}
	}
	c.mu.RUnlock()
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].rank != matches[j].rank {
			return matches[i].rank < matches[j].rank
		}
		if matches[i].instrument.TradingSymbol != matches[j].instrument.TradingSymbol {
			return matches[i].instrument.TradingSymbol < matches[j].instrument.TradingSymbol
		}
		return matches[i].instrument.Exchange < matches[j].instrument.Exchange
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	result := make([]domain.Instrument, len(matches))
	for index := range matches {
		result[index] = matches[index].instrument
	}
	return result
}

func (c *Catalog) Validate(exchange, symbol string) error {
	c.mu.RLock()
	_, ok := c.symbols[key(exchange, symbol)]
	loadedAt := c.loadedAt
	c.mu.RUnlock()
	if loadedAt.IsZero() {
		return errors.New("symbol catalog has not been loaded; log in to Kite and refresh symbols")
	}
	if !ok {
		return fmt.Errorf("%s:%s is not in the current Zerodha equity instrument master", strings.ToUpper(exchange), strings.ToUpper(symbol))
	}
	return nil
}

func (c *Catalog) LoadedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.loadedAt
}

func (c *Catalog) IsFresh(now time.Time, location *time.Location) bool {
	c.mu.RLock()
	loaded := c.loadedAt
	c.mu.RUnlock()
	if loaded.IsZero() {
		return false
	}
	y1, m1, d1 := loaded.In(location).Date()
	y2, m2, d2 := now.In(location).Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

func key(exchange, symbol string) string {
	return strings.ToUpper(strings.TrimSpace(exchange)) + "\x00" + strings.ToUpper(strings.TrimSpace(symbol))
}
