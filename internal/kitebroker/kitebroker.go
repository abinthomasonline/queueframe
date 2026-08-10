// Package kitebroker adapts the official Zerodha Go client to the application's
// broker interfaces.
package kitebroker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"github.com/abinthomasonline/queueframe/internal/broker"
	"github.com/abinthomasonline/queueframe/internal/domain"
)

const (
	// DefaultRequestTimeout matches the timeout in gokiteconnect. It bounds an
	// in-flight call because the SDK does not expose context-aware methods.
	DefaultRequestTimeout = 7 * time.Second
	appName               = "queueframe"
)

var decimalPattern = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+)$`)

// Config configures the SDK clients made by a Factory. HTTPClient and BaseURI
// are primarily useful for tests. All SDK clients made by a factory share the
// configured http.Client (and therefore its connection pool).
type Config struct {
	HTTPClient     *http.Client
	BaseURI        string
	RequestTimeout time.Duration
}

// Factory creates isolated SDK clients while sharing their HTTP transport.
// An SDK client must belong to exactly one Kite account; its access token is
// mutable and must never be swapped between accounts.
type Factory struct {
	httpClient *http.Client
	baseURI    string
}

// NewFactory returns a production factory with a shared keep-alive transport
// and a fixed seven-second request timeout.
func NewFactory() *Factory {
	return NewFactoryWithConfig(Config{})
}

// NewFactoryWithConfig returns a factory using cfg. A non-positive timeout is
// replaced with DefaultRequestTimeout. The supplied HTTP client is shallow
// copied so its Timeout is not mutated; its Transport remains shared.
func NewFactoryWithConfig(cfg Config) *Factory {
	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}

	var httpClient *http.Client
	if cfg.HTTPClient != nil {
		clientCopy := *cfg.HTTPClient
		clientCopy.Timeout = timeout
		clientCopy.CheckRedirect = rejectRedirect
		httpClient = &clientCopy
	} else {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = 100
		transport.MaxIdleConnsPerHost = 100
		httpClient = &http.Client{
			Transport:     transport,
			Timeout:       timeout,
			CheckRedirect: rejectRedirect,
		}
	}

	return &Factory{
		httpClient: httpClient,
		baseURI:    cfg.BaseURI,
	}
}

// Order placement is at-most-once at the wire boundary too. net/http normally
// replays POST bodies across 307/308 responses; returning ErrUseLastResponse
// prevents a broker or proxy redirect from creating a second placement POST.
func rejectRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

// LoginURL creates the official Kite login URL and carries state through
// redirect_params for callback-CSRF protection.
func (f *Factory) LoginURL(apiKey, state string) string {
	c := f.sdkClient(apiKey, "")
	return c.GetLoginURLWithparams(url.Values{"state": []string{state}})
}

// GenerateSession exchanges a one-use request token for an access token.
// The string argument order is intentionally named here to make the broker
// interface's otherwise anonymous tuple unambiguous:
//
//	GenerateSession(ctx, apiKey, apiSecret, requestToken)
func (f *Factory) GenerateSession(ctx context.Context, apiKey, apiSecret, requestToken string) (broker.Session, error) {
	if err := contextPrecheck(ctx); err != nil {
		return broker.Session{}, err
	}

	c := f.sdkClient(apiKey, "")
	session, err := c.GenerateSession(requestToken, apiSecret)
	if err != nil {
		return broker.Session{}, err
	}

	return broker.Session{
		UserID:      session.UserID,
		APIKey:      session.APIKey,
		AccessToken: session.AccessToken,
		LoginAt:     session.LoginTime.Time,
	}, nil
}

// Client returns an account-scoped broker client. Calls on the returned client
// only check context before entering gokiteconnect. Once a request starts, the
// SDK cannot attach or observe the context, so the HTTP timeout is the bound and
// the authoritative SDK result is always returned.
func (f *Factory) Client(apiKey, accessToken string) broker.Client {
	return &client{sdk: f.sdkClient(apiKey, accessToken)}
}

func (f *Factory) sdkClient(apiKey, accessToken string) *kiteconnect.Client {
	c := kiteconnect.New(apiKey)
	c.SetHTTPClient(f.httpClient)
	c.SetDebug(false) // Debug mode includes the Authorization header in logs.
	c.SetAppName(appName)
	if f.baseURI != "" {
		c.SetBaseURI(f.baseURI)
	}
	if accessToken != "" {
		c.SetAccessToken(accessToken)
	}
	return c
}

type client struct {
	sdk *kiteconnect.Client
}

func (c *client) Profile(ctx context.Context) (string, error) {
	if err := contextPrecheck(ctx); err != nil {
		return "", err
	}
	profile, err := c.sdk.GetUserProfile()
	if err != nil {
		return "", err
	}
	return profile.UserID, nil
}

func (c *client) Instruments(ctx context.Context, exchange string) ([]domain.Instrument, error) {
	if err := contextPrecheck(ctx); err != nil {
		return nil, err
	}
	instruments, err := c.sdk.GetInstrumentsByExchange(exchange)
	if err != nil {
		return nil, err
	}

	result := make([]domain.Instrument, 0, len(instruments))
	for _, instrument := range instruments {
		result = append(result, domain.Instrument{
			Exchange:       instrument.Exchange,
			TradingSymbol:  instrument.Tradingsymbol,
			Name:           instrument.Name,
			Segment:        instrument.Segment,
			InstrumentType: instrument.InstrumentType,
			Expiry:         instrument.Expiry.Time,
		})
	}
	return result, nil
}

func (c *client) PlaceRegularOrder(ctx context.Context, params domain.OrderParams, tag string) (broker.Order, error) {
	if err := contextPrecheck(ctx); err != nil {
		return broker.Order{}, err
	}

	price, err := parseDecimal("price", params.Price)
	if err != nil {
		return broker.Order{}, inputPlaceError(err)
	}
	triggerPrice, err := parseDecimal("trigger price", params.TriggerPrice)
	if err != nil {
		return broker.Order{}, inputPlaceError(err)
	}
	marketProtection, err := parseMarketProtection(params.MarketProtection)
	if err != nil {
		return broker.Order{}, inputPlaceError(err)
	}

	response, err := c.sdk.PlaceOrder(kiteconnect.VarietyRegular, kiteconnect.OrderParams{
		Exchange:          params.Exchange,
		Tradingsymbol:     params.TradingSymbol,
		Validity:          params.Validity,
		ValidityTTL:       params.ValidityTTL,
		Product:           params.Product,
		OrderType:         params.OrderType,
		TransactionType:   params.TransactionType,
		Quantity:          params.Quantity,
		DisclosedQuantity: params.DisclosedQuantity,
		Price:             price,
		TriggerPrice:      triggerPrice,
		MarketProtection:  marketProtection,
		Autoslice:         params.Autoslice,
		Tag:               tag,
	})
	if err != nil {
		return broker.Order{}, mapPlaceError(err)
	}
	if response.OrderID == "" {
		return broker.Order{}, &broker.PlaceError{
			Code: http.StatusGatewayTimeout, Type: kiteconnect.DataError,
			Message: "Kite returned a successful order response without an order ID", Ambiguous: true,
		}
	}

	mapped := broker.Order{
		OrderID: response.OrderID,
		Tag:     tag,
		Params:  params,
	}
	for _, child := range response.Children {
		item := broker.ChildOrder{OrderID: child.OrderID}
		if child.Error != nil {
			item.ErrorCode = child.Error.Code
			item.ErrorType = child.Error.ErrorType
			item.ErrorMessage = child.Error.Message
		}
		mapped.Children = append(mapped.Children, item)
	}
	return mapped, nil
}

func (c *client) Orders(ctx context.Context) ([]broker.Order, error) {
	if err := contextPrecheck(ctx); err != nil {
		return nil, err
	}
	orders, err := c.sdk.GetOrders()
	if err != nil {
		return nil, err
	}

	result := make([]broker.Order, 0, len(orders))
	for _, order := range orders {
		mapped, err := mapOrder(order)
		if err != nil {
			return nil, fmt.Errorf("map Kite order %q: %w", order.OrderID, err)
		}
		result = append(result, mapped)
	}
	return result, nil
}

func (c *client) InitiateHoldingsAuthorization(ctx context.Context) (string, error) {
	if err := contextPrecheck(ctx); err != nil {
		return "", err
	}
	response, err := c.sdk.InitiateHoldingsAuth(kiteconnect.HoldingAuthParams{})
	if err != nil {
		return "", err
	}
	return response.RedirectURL, nil
}

func mapOrder(order kiteconnect.Order) (broker.Order, error) {
	quantity, err := nonNegativeInt(order.Quantity)
	if err != nil {
		return broker.Order{}, fmt.Errorf("quantity: %w", err)
	}
	disclosedQuantity, err := nonNegativeInt(order.DisclosedQuantity)
	if err != nil {
		return broker.Order{}, fmt.Errorf("disclosed quantity: %w", err)
	}
	if math.IsNaN(order.MarketProtection) || math.IsInf(order.MarketProtection, 0) {
		return broker.Order{}, fmt.Errorf("market protection: %v is not finite", order.MarketProtection)
	}

	return broker.Order{
		OrderID:                 order.OrderID,
		ExchangeOrderID:         order.ExchangeOrderID,
		Status:                  order.Status,
		StatusMessage:           order.StatusMessage,
		OrderTimestamp:          order.OrderTimestamp.Time,
		ExchangeTimestamp:       order.ExchangeTimestamp.Time,
		ExchangeUpdateTimestamp: order.ExchangeUpdateTimestamp.Time,
		Tag:                     order.Tag,
		Params: domain.OrderParams{
			Exchange:          order.Exchange,
			TradingSymbol:     order.TradingSymbol,
			TransactionType:   order.TransactionType,
			Product:           order.Product,
			OrderType:         order.OrderType,
			Quantity:          quantity,
			DisclosedQuantity: disclosedQuantity,
			Price:             formatDecimal(order.Price),
			TriggerPrice:      formatDecimal(order.TriggerPrice),
			Validity:          order.Validity,
			ValidityTTL:       order.ValidityTTL,
			MarketProtection:  formatDecimal(order.MarketProtection),
		},
	}, nil
}

func parseDecimal(field, value string) (float64, error) {
	if value == "" {
		return 0, nil
	}
	if !decimalPattern.MatchString(value) {
		return 0, fmt.Errorf("%s %q is not a non-negative decimal", field, value)
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
		return 0, fmt.Errorf("%s %q cannot be represented as a finite number", field, value)
	}
	return parsed, nil
}

func parseMarketProtection(value string) (float64, error) {
	if value == "-1" {
		return -1, nil
	}
	return parseDecimal("market protection", value)
}

func formatDecimal(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func nonNegativeInt(value float64) (int, error) {
	if value < 0 {
		return 0, fmt.Errorf("%v is negative", value)
	}
	return signedInt(value)
}

func signedInt(value float64) (int, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value {
		return 0, fmt.Errorf("%v is not an integer", value)
	}
	if strconv.IntSize == 32 && (value < math.MinInt32 || value > math.MaxInt32) {
		return 0, fmt.Errorf("%v is outside the int range", value)
	}
	// float64 can represent the signed 64-bit boundary only as +/-2^63, so
	// compare with an exclusive upper bound before conversion.
	if strconv.IntSize == 64 && (value < -math.Exp2(63) || value >= math.Exp2(63)) {
		return 0, fmt.Errorf("%v is outside the int range", value)
	}
	return int(value), nil
}

func inputPlaceError(err error) error {
	return &broker.PlaceError{
		Code:      http.StatusBadRequest,
		Type:      kiteconnect.InputError,
		Message:   err.Error(),
		Ambiguous: false,
	}
}

func mapPlaceError(err error) error {
	var kiteError kiteconnect.Error
	if errors.As(err, &kiteError) {
		return &broker.PlaceError{
			Code:    kiteError.Code,
			Type:    kiteError.ErrorType,
			Message: kiteError.Message,
			Ambiguous: (kiteError.Code >= http.StatusMultipleChoices && kiteError.Code < http.StatusBadRequest) ||
				kiteError.Code >= http.StatusInternalServerError ||
				kiteError.ErrorType == kiteconnect.NetworkError ||
				kiteError.ErrorType == kiteconnect.DataError,
		}
	}

	var networkError net.Error
	if errors.As(err, &networkError) {
		return &broker.PlaceError{
			Code:      http.StatusServiceUnavailable,
			Type:      kiteconnect.NetworkError,
			Message:   err.Error(),
			Ambiguous: true,
		}
	}

	// After PlaceOrder begins, an unclassified error cannot prove that Kite did
	// not accept the order. Reconciliation by the unique tag is required.
	return &broker.PlaceError{
		Type:      "UnknownException",
		Message:   err.Error(),
		Ambiguous: true,
	}
}

func contextPrecheck(ctx context.Context) error {
	if ctx == nil {
		return errors.New("kitebroker: nil context")
	}
	return ctx.Err()
}

var (
	_ broker.Factory = (*Factory)(nil)
	_ broker.Client  = (*client)(nil)
)
