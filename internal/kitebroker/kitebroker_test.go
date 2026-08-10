package kitebroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"github.com/abinthomasonline/queueframe/internal/broker"
	"github.com/abinthomasonline/queueframe/internal/domain"
)

func TestLoginURLCarriesState(t *testing.T) {
	t.Parallel()

	loginURL := NewFactory().LoginURL("test-key", "single-use-state")
	parsed, err := url.Parse(loginURL)
	if err != nil {
		t.Fatalf("parse login URL: %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "kite.zerodha.com" || parsed.Path != "/connect/login" {
		t.Fatalf("unexpected login endpoint: %s", parsed.String())
	}
	if got := parsed.Query().Get("api_key"); got != "test-key" {
		t.Fatalf("api_key = %q, want test-key", got)
	}

	redirectParams, err := url.ParseQuery(parsed.Query().Get("redirect_params"))
	if err != nil {
		t.Fatalf("parse redirect_params: %v", err)
	}
	if got := redirectParams.Get("state"); got != "single-use-state" {
		t.Fatalf("state = %q, want single-use-state", got)
	}
}

func TestGenerateSession(t *testing.T) {
	t.Parallel()

	const (
		apiKey       = "api-key"
		apiSecret    = "api-secret"
		requestToken = "request-token"
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/session/token" {
			t.Errorf("request = %s %s, want POST /session/token", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		checksumBytes := sha256.Sum256([]byte(apiKey + requestToken + apiSecret))
		wantChecksum := hex.EncodeToString(checksumBytes[:])
		assertFormValue(t, r.Form, "api_key", apiKey)
		assertFormValue(t, r.Form, "request_token", requestToken)
		assertFormValue(t, r.Form, "checksum", wantChecksum)

		writeEnvelope(t, w, map[string]any{
			"user_id":      "AB1234",
			"api_key":      apiKey,
			"access_token": "access-token",
			"login_time":   "2026-08-07 08:31:02",
		})
	}))
	t.Cleanup(server.Close)

	factory := testFactory(server, time.Second)
	session, err := factory.GenerateSession(context.Background(), apiKey, apiSecret, requestToken)
	if err != nil {
		t.Fatalf("GenerateSession: %v", err)
	}
	wantLoginAt, err := time.ParseInLocation("2006-01-02 15:04:05", "2026-08-07 08:31:02", mustKolkata(t))
	if err != nil {
		t.Fatalf("parse expected login time: %v", err)
	}
	want := broker.Session{
		UserID:      "AB1234",
		APIKey:      apiKey,
		AccessToken: "access-token",
		LoginAt:     wantLoginAt,
	}
	if !reflect.DeepEqual(session, want) {
		t.Fatalf("session = %#v, want %#v", session, want)
	}
}

func TestProfileAndInstruments(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token api-key:access-token" {
			t.Errorf("Authorization = %q", got)
		}
		if strings.Contains(r.Header.Get("User-Agent"), "queueframe") == false {
			t.Errorf("User-Agent = %q, want app name", r.Header.Get("User-Agent"))
		}

		switch r.URL.Path {
		case "/user/profile":
			writeEnvelope(t, w, map[string]any{"user_id": "AB1234"})
		case "/instruments/NSE":
			w.Header().Set("Content-Type", "text/csv")
			_, _ = fmt.Fprintln(w, "instrument_token,exchange_token,tradingsymbol,name,last_price,expiry,strike,tick_size,lot_size,instrument_type,segment,exchange")
			_, _ = fmt.Fprintln(w, "123,456,ACME,Acme Ltd,99.5,2026-08-28,0,0.05,1,EQ,NSE,NSE")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := testFactory(server, time.Second).Client("api-key", "access-token")
	userID, err := client.Profile(context.Background())
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if userID != "AB1234" {
		t.Fatalf("user ID = %q, want AB1234", userID)
	}

	instruments, err := client.Instruments(context.Background(), "NSE")
	if err != nil {
		t.Fatalf("Instruments: %v", err)
	}
	if len(instruments) != 1 {
		t.Fatalf("instrument count = %d, want 1", len(instruments))
	}
	want := domain.Instrument{
		Exchange:       "NSE",
		TradingSymbol:  "ACME",
		Name:           "Acme Ltd",
		Segment:        "NSE",
		InstrumentType: "EQ",
		Expiry:         time.Date(2026, time.August, 28, 0, 0, 0, 0, mustKolkata(t)),
	}
	if !reflect.DeepEqual(instruments[0], want) {
		t.Fatalf("instrument = %#v, want %#v", instruments[0], want)
	}
}

func TestPlaceRegularOrderMapsDirectPreOpenParameters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		params     domain.OrderParams
		wantForm   map[string]string
		wantAbsent []string
	}{
		{
			name: "limit",
			params: domain.OrderParams{
				Exchange: "NSE", TradingSymbol: "ACME", TransactionType: "SELL", Product: "CNC",
				OrderType: "LIMIT", Quantity: 17, DisclosedQuantity: 5, Price: "0012.3400",
				TriggerPrice: "0", Validity: "DAY", MarketProtection: "0",
			},
			wantForm: map[string]string{
				"exchange": "NSE", "tradingsymbol": "ACME", "transaction_type": "SELL", "product": "CNC",
				"order_type": "LIMIT", "quantity": "17", "disclosed_quantity": "5", "price": "12.34",
				"validity": "DAY", "tag": "SATOPEN0001",
			},
			wantAbsent: []string{"trigger_price", "validity_ttl", "market_protection", "autoslice"},
		},
		{
			name: "market with fractional protection",
			params: domain.OrderParams{
				Exchange: "BSE", TradingSymbol: "ACME", TransactionType: "BUY", Product: "MIS",
				OrderType: "MARKET", Quantity: 2, Price: "0", TriggerPrice: "", Validity: "DAY",
				MarketProtection: ".3",
			},
			wantForm: map[string]string{
				"exchange": "BSE", "tradingsymbol": "ACME", "transaction_type": "BUY", "product": "MIS",
				"order_type": "MARKET", "quantity": "2", "validity": "DAY", "market_protection": "0.3",
				"tag": "SATOPEN0001",
			},
			wantAbsent: []string{"price", "trigger_price", "validity_ttl", "autoslice"},
		},
		{
			name: "market with automatic protection",
			params: domain.OrderParams{
				Exchange: "NSE", TradingSymbol: "ACME", TransactionType: "SELL", Product: "CNC",
				OrderType: "MARKET", Quantity: 1, Validity: "DAY", MarketProtection: "-1",
			},
			wantForm: map[string]string{
				"exchange": "NSE", "tradingsymbol": "ACME", "transaction_type": "SELL", "product": "CNC",
				"order_type": "MARKET", "quantity": "1", "validity": "DAY", "market_protection": "-1",
				"tag": "SATOPEN0001",
			},
			wantAbsent: []string{"price", "trigger_price", "validity_ttl", "autoslice"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			forms := make(chan url.Values, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/orders/regular" {
					t.Errorf("request = %s %s, want POST /orders/regular", r.Method, r.URL.Path)
				}
				if err := r.ParseForm(); err != nil {
					t.Errorf("parse form: %v", err)
					return
				}
				forms <- r.Form
				writeEnvelope(t, w, map[string]any{"order_id": "order-123"})
			}))
			t.Cleanup(server.Close)

			client := testFactory(server, time.Second).Client("api-key", "access-token")
			order, err := client.PlaceRegularOrder(context.Background(), test.params, "SATOPEN0001")
			if err != nil {
				t.Fatalf("PlaceRegularOrder: %v", err)
			}
			if order.OrderID != "order-123" || order.Tag != "SATOPEN0001" || !reflect.DeepEqual(order.Params, test.params) {
				t.Fatalf("order = %#v", order)
			}

			form := <-forms
			for key, want := range test.wantForm {
				assertFormValue(t, form, key, want)
			}
			for _, key := range test.wantAbsent {
				if _, exists := form[key]; exists {
					t.Errorf("form[%q] = %q, want omitted", key, form.Get(key))
				}
			}
		})
	}
}

func TestPlaceRegularOrderRejectsInvalidDecimalsBeforeHTTP(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		writeEnvelope(t, w, map[string]any{"order_id": "should-not-happen"})
	}))
	t.Cleanup(server.Close)
	client := testFactory(server, time.Second).Client("key", "token")

	for _, value := range []string{"-1", "1e3", "NaN", " 1", "1 ", "1.", "+1", strings.Repeat("9", 400)} {
		t.Run(value, func(t *testing.T) {
			_, err := client.PlaceRegularOrder(context.Background(), domain.OrderParams{Price: value}, "tag")
			var placeError *broker.PlaceError
			if !errors.As(err, &placeError) {
				t.Fatalf("error = %T %v, want *broker.PlaceError", err, err)
			}
			if placeError.Code != http.StatusBadRequest || placeError.Type != kiteconnect.InputError || placeError.Ambiguous {
				t.Fatalf("PlaceError = %#v", placeError)
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("HTTP request count = %d, want 0", got)
	}
}

func TestPlaceRegularOrderMapsKiteErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		errorType string
		ambiguous bool
	}{
		{name: "order rejected", status: http.StatusBadRequest, errorType: kiteconnect.OrderError},
		{name: "token rejected", status: http.StatusForbidden, errorType: kiteconnect.TokenError},
		{name: "rate limited", status: http.StatusTooManyRequests, errorType: kiteconnect.GeneralError},
		{name: "server error", status: http.StatusInternalServerError, errorType: kiteconnect.GeneralError, ambiguous: true},
		{name: "network error", status: http.StatusServiceUnavailable, errorType: kiteconnect.NetworkError, ambiguous: true},
		{name: "data error", status: http.StatusGatewayTimeout, errorType: kiteconnect.DataError, ambiguous: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status":     "error",
					"error_type": test.errorType,
					"message":    "broker said no",
				})
			}))
			t.Cleanup(server.Close)

			client := testFactory(server, time.Second).Client("key", "token")
			_, err := client.PlaceRegularOrder(context.Background(), domain.OrderParams{}, "tag")
			var placeError *broker.PlaceError
			if !errors.As(err, &placeError) {
				t.Fatalf("error = %T %v, want *broker.PlaceError", err, err)
			}
			if placeError.Code != test.status || placeError.Type != test.errorType || placeError.Message != "broker said no" || placeError.Ambiguous != test.ambiguous {
				t.Fatalf("PlaceError = %#v", placeError)
			}
		})
	}
}

func TestPlaceRegularOrderTreatsMalformedSuccessAndTimeoutAsAmbiguous(t *testing.T) {
	t.Parallel()

	t.Run("malformed success", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("not-json"))
		}))
		t.Cleanup(server.Close)

		client := testFactory(server, time.Second).Client("key", "token")
		_, err := client.PlaceRegularOrder(context.Background(), domain.OrderParams{}, "tag")
		assertAmbiguousPlaceError(t, err, kiteconnect.DataError)
	})

	t.Run("missing order ID", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeEnvelope(t, w, map[string]any{})
		}))
		t.Cleanup(server.Close)

		client := testFactory(server, time.Second).Client("key", "token")
		_, err := client.PlaceRegularOrder(context.Background(), domain.OrderParams{}, "tag")
		assertAmbiguousPlaceError(t, err, kiteconnect.DataError)
	})

	t.Run("transport timeout", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(40 * time.Millisecond)
			writeEnvelope(t, w, map[string]any{"order_id": "late-order"})
		}))
		t.Cleanup(server.Close)

		client := testFactory(server, 5*time.Millisecond).Client("key", "token")
		_, err := client.PlaceRegularOrder(context.Background(), domain.OrderParams{}, "tag")
		assertAmbiguousPlaceError(t, err, kiteconnect.NetworkError)
	})
}

func TestPlaceRegularOrderNeverFollowsPOSTRedirect(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/orders/regular" {
			w.Header().Set("Location", "/orders/replayed")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTemporaryRedirect)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "error", "error_type": kiteconnect.NetworkError, "message": "redirected placement",
			})
			return
		}
		writeEnvelope(t, w, map[string]any{"order_id": "duplicate-order"})
	}))
	t.Cleanup(server.Close)

	client := testFactory(server, time.Second).Client("key", "token")
	_, err := client.PlaceRegularOrder(context.Background(), domain.OrderParams{}, "tag")
	var placeError *broker.PlaceError
	if !errors.As(err, &placeError) || !placeError.Ambiguous {
		t.Fatalf("error = %T %#v, want ambiguous placement error", err, err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("wire request count = %d, want exactly one", got)
	}
}

func TestContextIsCheckedBeforeRequest(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		writeEnvelope(t, w, map[string]any{"order_id": "unexpected"})
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := testFactory(server, time.Second).Client("key", "token")
	_, err := client.PlaceRegularOrder(ctx, domain.OrderParams{}, "tag")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("HTTP request count = %d, want 0", got)
	}
}

func TestCancellationAfterRequestStartsDoesNotHideAuthoritativeResult(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		writeEnvelope(t, w, map[string]any{"order_id": "accepted-order"})
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan struct {
		order broker.Order
		err   error
	}, 1)
	client := testFactory(server, time.Second).Client("key", "token")
	go func() {
		order, err := client.PlaceRegularOrder(ctx, domain.OrderParams{}, "tag")
		result <- struct {
			order broker.Order
			err   error
		}{order: order, err: err}
	}()

	<-started
	cancel()
	close(release)
	got := <-result
	if got.err != nil || got.order.OrderID != "accepted-order" {
		t.Fatalf("result = (%#v, %v), want accepted order", got.order, got.err)
	}
}

func TestOrdersMapsBrokerFields(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/orders" {
			t.Errorf("request = %s %s, want GET /orders", r.Method, r.URL.Path)
		}
		writeEnvelope(t, w, []map[string]any{{
			"order_id":                  "order-1",
			"exchange_order_id":         "exchange-1",
			"status":                    "REJECTED",
			"status_message":            "insufficient holdings",
			"order_timestamp":           "2026-08-07 09:00:01",
			"exchange_timestamp":        "2026-08-07 09:00:02",
			"exchange_update_timestamp": "2026-08-07 09:00:03",
			"tag":                       "SATOPEN0001",
			"exchange":                  "NSE",
			"tradingsymbol":             "ACME",
			"transaction_type":          "SELL",
			"product":                   "CNC",
			"order_type":                "LIMIT",
			"quantity":                  17,
			"disclosed_quantity":        5,
			"price":                     12.34,
			"trigger_price":             0.25,
			"validity":                  "DAY",
			"validity_ttl":              0,
			"market_protection":         3.5,
		}})
	}))
	t.Cleanup(server.Close)

	client := testFactory(server, time.Second).Client("key", "token")
	orders, err := client.Orders(context.Background())
	if err != nil {
		t.Fatalf("Orders: %v", err)
	}
	location, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	want := []broker.Order{{
		OrderID:                 "order-1",
		ExchangeOrderID:         "exchange-1",
		Status:                  "REJECTED",
		StatusMessage:           "insufficient holdings",
		OrderTimestamp:          time.Date(2026, 8, 7, 9, 0, 1, 0, location),
		ExchangeTimestamp:       time.Date(2026, 8, 7, 9, 0, 2, 0, location),
		ExchangeUpdateTimestamp: time.Date(2026, 8, 7, 9, 0, 3, 0, location),
		Tag:                     "SATOPEN0001",
		Params: domain.OrderParams{
			Exchange:          "NSE",
			TradingSymbol:     "ACME",
			TransactionType:   "SELL",
			Product:           "CNC",
			OrderType:         "LIMIT",
			Quantity:          17,
			DisclosedQuantity: 5,
			Price:             "12.34",
			TriggerPrice:      "0.25",
			Validity:          "DAY",
			MarketProtection:  "3.5",
		},
	}}
	if !reflect.DeepEqual(orders, want) {
		t.Fatalf("orders = %#v, want %#v", orders, want)
	}
}

func TestInitiateHoldingsAuthorization(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/portfolio/holdings/authorise" {
			t.Errorf("request = %s %s, want POST /portfolio/holdings/authorise", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if len(r.Form) != 0 {
			t.Errorf("authorization form = %v, want empty (all holdings)", r.Form)
		}
		writeEnvelope(t, w, map[string]any{"request_id": "request-1"})
	}))
	t.Cleanup(server.Close)

	client := testFactory(server, time.Second).Client("api-key", "access-token")
	redirectURL, err := client.InitiateHoldingsAuthorization(context.Background())
	if err != nil {
		t.Fatalf("InitiateHoldingsAuthorization: %v", err)
	}
	want := "https://kite.zerodha.com/connect/portfolio/authorise/holdings/api-key/request-1"
	if redirectURL != want {
		t.Fatalf("redirect URL = %q, want %q", redirectURL, want)
	}
}

func TestMapOrderRejectsNonIntegralQuantity(t *testing.T) {
	t.Parallel()

	_, err := mapOrder(kiteconnect.Order{OrderID: "bad-order", Quantity: 1.5})
	if err == nil || !strings.Contains(err.Error(), "quantity") {
		t.Fatalf("error = %v, want quantity mapping error", err)
	}
}

func testFactory(server *httptest.Server, timeout time.Duration) *Factory {
	return NewFactoryWithConfig(Config{
		HTTPClient:     server.Client(),
		BaseURI:        server.URL,
		RequestTimeout: timeout,
	})
}

func writeEnvelope(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": data}); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func assertFormValue(t *testing.T, form url.Values, key, want string) {
	t.Helper()
	if got := form.Get(key); got != want {
		t.Errorf("form[%q] = %q, want %q", key, got, want)
	}
}

func assertAmbiguousPlaceError(t *testing.T, err error, wantType string) {
	t.Helper()
	var placeError *broker.PlaceError
	if !errors.As(err, &placeError) {
		t.Fatalf("error = %T %v, want *broker.PlaceError", err, err)
	}
	if !placeError.Ambiguous || placeError.Type != wantType {
		t.Fatalf("PlaceError = %#v, want ambiguous %s", placeError, wantType)
	}
}

func mustKolkata(t *testing.T) *time.Location {
	t.Helper()
	location, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("load Asia/Kolkata: %v", err)
	}
	return location
}
