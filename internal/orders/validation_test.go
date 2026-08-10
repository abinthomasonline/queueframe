package orders

import (
	"strings"
	"testing"

	"github.com/abinthomasonline/queueframe/internal/domain"
)

func TestValidateParamsAcceptsDirectPreOpenPayloads(t *testing.T) {
	tests := []struct {
		name   string
		params domain.OrderParams
	}{
		{
			name: "limit",
			params: domain.OrderParams{
				Exchange: "NSE", TradingSymbol: "ABC", TransactionType: "SELL", Product: "CNC",
				OrderType: "LIMIT", Quantity: 10, DisclosedQuantity: 4, Price: "12.3500",
				TriggerPrice: "0", Validity: "DAY", MarketProtection: "0",
			},
		},
		{
			name: "market with automatic protection",
			params: domain.OrderParams{
				Exchange: "BSE", TradingSymbol: "ABC", TransactionType: "BUY", Product: "MIS",
				OrderType: "MARKET", Quantity: 1, Price: "", TriggerPrice: "", Validity: "DAY",
				MarketProtection: "-1",
			},
		},
		{
			name: "market with fractional protection percentage",
			params: domain.OrderParams{
				Exchange: "NSE", TradingSymbol: "ABC", TransactionType: "SELL", Product: "CNC",
				OrderType: "MARKET", Quantity: 1, Price: ".0", TriggerPrice: "0.00", Validity: "DAY",
				MarketProtection: ".3",
			},
		},
		{
			name: "market with maximum whole protection percentage",
			params: domain.OrderParams{
				Exchange: "NSE", TradingSymbol: "ABC", TransactionType: "SELL", Product: "CNC",
				OrderType: "MARKET", Quantity: 1, Validity: "DAY", MarketProtection: "100.000",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateParams(test.params); err != nil {
				t.Fatalf("ValidateParams: %v", err)
			}
		})
	}
}

func TestValidateParamsRejectsUnsupportedOrInconsistentPayloads(t *testing.T) {
	validLimit := domain.OrderParams{
		Exchange: "NSE", TradingSymbol: "ABC", TransactionType: "SELL", Product: "CNC",
		OrderType: "LIMIT", Quantity: 10, Price: "12.35", Validity: "DAY",
	}
	validMarket := domain.OrderParams{
		Exchange: "NSE", TradingSymbol: "ABC", TransactionType: "SELL", Product: "CNC",
		OrderType: "MARKET", Quantity: 10, Validity: "DAY", MarketProtection: "-1",
	}

	tests := []struct {
		name    string
		params  domain.OrderParams
		wantErr string
	}{
		{name: "stop-limit order", params: withOrderType(validLimit, "SL"), wantErr: "LIMIT or MARKET"},
		{name: "stop-market order", params: withOrderType(validLimit, "SL-M"), wantErr: "LIMIT or MARKET"},
		{name: "zero quantity", params: withQuantity(validLimit, 0), wantErr: "quantity must be positive"},
		{name: "negative quantity", params: withQuantity(validLimit, -1), wantErr: "quantity must be positive"},
		{name: "negative disclosed quantity", params: withDisclosedQuantity(validLimit, -1), wantErr: "disclosed quantity"},
		{name: "disclosed quantity above quantity", params: withDisclosedQuantity(validLimit, 11), wantErr: "disclosed quantity"},
		{name: "TTL on DAY order", params: withValidityTTL(validLimit, 1), wantErr: "validity TTL must be 0"},
		{name: "negative TTL on DAY order", params: withValidityTTL(validLimit, -1), wantErr: "validity TTL must be 0"},
		{name: "autoslice", params: withAutoslice(validLimit), wantErr: "autoslice is disabled"},
		{name: "LIMIT missing price", params: withPrice(validLimit, ""), wantErr: "price must be a positive decimal"},
		{name: "LIMIT zero price", params: withPrice(validLimit, "0.00"), wantErr: "price must be a positive decimal"},
		{name: "LIMIT nonzero trigger", params: withTriggerPrice(validLimit, "12"), wantErr: "trigger price must be empty or 0"},
		{name: "LIMIT automatic market protection", params: withMarketProtection(validLimit, "-1"), wantErr: "0 (omitted) for LIMIT"},
		{name: "LIMIT custom market protection", params: withMarketProtection(validLimit, "0.3"), wantErr: "0 (omitted) for LIMIT"},
		{name: "LIMIT rational zero protection", params: withMarketProtection(validLimit, "0/1"), wantErr: "0 (omitted) for LIMIT"},
		{name: "LIMIT signed zero protection", params: withMarketProtection(validLimit, "-0"), wantErr: "0 (omitted) for LIMIT"},
		{name: "MARKET nonzero price", params: withPrice(validMarket, "12"), wantErr: "price must be empty or 0"},
		{name: "MARKET nonzero trigger", params: withTriggerPrice(validMarket, "12"), wantErr: "trigger price must be empty or 0"},
		{name: "MARKET omitted protection", params: withMarketProtection(validMarket, ""), wantErr: "decimal percentage greater than 0"},
		{name: "MARKET zero protection", params: withMarketProtection(validMarket, "0"), wantErr: "decimal percentage greater than 0"},
		{name: "MARKET protection below automatic sentinel", params: withMarketProtection(validMarket, "-2"), wantErr: "decimal percentage greater than 0"},
		{name: "MARKET noncanonical automatic protection", params: withMarketProtection(validMarket, "-1.0"), wantErr: "decimal percentage greater than 0"},
		{name: "MARKET protection above maximum", params: withMarketProtection(validMarket, "100.0001"), wantErr: "decimal percentage greater than 0"},
		{name: "MARKET protection exponent", params: withMarketProtection(validMarket, "1e-1"), wantErr: "decimal percentage greater than 0"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateParams(test.params)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ValidateParams error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestValidateParamsRejectsDecimalsTheAdapterCannotSend(t *testing.T) {
	validMarket := domain.OrderParams{
		Exchange: "NSE", TradingSymbol: "ABC", TransactionType: "SELL", Product: "CNC",
		OrderType: "MARKET", Quantity: 10, Validity: "DAY", MarketProtection: "-1",
	}
	for _, field := range []string{"price", "trigger price"} {
		for _, value := range []string{"NaN", "Inf", "1e3", "-1", "1.", "+1"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				params := validMarket
				if field == "price" {
					params.Price = value
				} else {
					params.TriggerPrice = value
				}
				if err := ValidateParams(params); err == nil {
					t.Fatalf("%s %q was accepted", field, value)
				}
			})
		}
	}
}

func TestValidateParamsRejectsDecimalValuesChangedByFloat64Conversion(t *testing.T) {
	validLimit := domain.OrderParams{
		Exchange: "NSE", TradingSymbol: "ABC", TransactionType: "SELL", Product: "CNC",
		OrderType: "LIMIT", Quantity: 1, Price: "10", Validity: "DAY", MarketProtection: "0",
	}
	for _, value := range []string{"9007199254740993", "0.3000000000000000000001"} {
		params := validLimit
		params.Price = value
		if err := ValidateParams(params); err == nil || !strings.Contains(err.Error(), "without changing its decimal value") {
			t.Fatalf("price %q error = %v, want precision error", value, err)
		}
	}
}

func withOrderType(params domain.OrderParams, value string) domain.OrderParams {
	params.OrderType = value
	return params
}

func withQuantity(params domain.OrderParams, value int) domain.OrderParams {
	params.Quantity = value
	return params
}

func withDisclosedQuantity(params domain.OrderParams, value int) domain.OrderParams {
	params.DisclosedQuantity = value
	return params
}

func withValidityTTL(params domain.OrderParams, value int) domain.OrderParams {
	params.ValidityTTL = value
	return params
}

func withAutoslice(params domain.OrderParams) domain.OrderParams {
	params.Autoslice = true
	return params
}

func withPrice(params domain.OrderParams, value string) domain.OrderParams {
	params.Price = value
	return params
}

func withTriggerPrice(params domain.OrderParams, value string) domain.OrderParams {
	params.TriggerPrice = value
	return params
}

func withMarketProtection(params domain.OrderParams, value string) domain.OrderParams {
	params.MarketProtection = value
	return params
}
