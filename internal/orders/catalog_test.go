package orders

import (
	"context"
	"testing"

	"github.com/abinthomasonline/queueframe/internal/broker"
	"github.com/abinthomasonline/queueframe/internal/domain"
)

type catalogClient struct{ broker.Client }

func (catalogClient) Instruments(_ context.Context, exchange string) ([]domain.Instrument, error) {
	if exchange == "NSE" {
		return []domain.Instrument{
			{Exchange: "NSE", TradingSymbol: "RELIANCE", Name: "Reliance Industries", InstrumentType: "EQ"},
			{Exchange: "NSE", TradingSymbol: "RELINFRA", Name: "Reliance Infrastructure", InstrumentType: "EQ"},
			{Exchange: "NSE", TradingSymbol: "NIFTY26AUGFUT", Name: "Nifty", InstrumentType: "FUT"},
		}, nil
	}
	return []domain.Instrument{
		{Exchange: "BSE", TradingSymbol: "500325", Name: "Reliance Industries", InstrumentType: "EQ"},
	}, nil
}

func TestCatalogSearchRanksSymbolsAndFiltersNonEquity(t *testing.T) {
	catalog := NewCatalog()
	if err := catalog.Refresh(context.Background(), catalogClient{}); err != nil {
		t.Fatal(err)
	}
	results := catalog.Search("reliance", 10)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3: %#v", len(results), results)
	}
	if results[0].TradingSymbol != "RELIANCE" {
		t.Fatalf("exact symbol match was not ranked first: %#v", results)
	}
	for _, result := range results {
		if result.TradingSymbol == "NIFTY26AUGFUT" {
			t.Fatal("non-equity instrument appeared in search")
		}
	}
}

func TestCatalogSearchAppliesLimitAndReturnsCopy(t *testing.T) {
	catalog := NewCatalog()
	if err := catalog.Refresh(context.Background(), catalogClient{}); err != nil {
		t.Fatal(err)
	}
	results := catalog.Search("rel", 1)
	if len(results) != 1 || results[0].TradingSymbol != "RELIANCE" {
		t.Fatalf("unexpected limited results: %#v", results)
	}
	results[0].TradingSymbol = "MUTATED"
	if err := catalog.Validate("NSE", "RELIANCE"); err != nil {
		t.Fatalf("search result mutated catalog: %v", err)
	}
}
