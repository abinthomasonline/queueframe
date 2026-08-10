package broker

import (
	"context"
	"time"

	"github.com/abinthomasonline/queueframe/internal/domain"
)

type Session struct {
	UserID      string
	APIKey      string
	AccessToken string
	LoginAt     time.Time
}

type Order struct {
	OrderID                 string
	ExchangeOrderID         string
	Status                  string
	StatusMessage           string
	OrderTimestamp          time.Time
	ExchangeTimestamp       time.Time
	ExchangeUpdateTimestamp time.Time
	Tag                     string
	Params                  domain.OrderParams
	Children                []ChildOrder
}

type ChildOrder struct {
	OrderID      string
	ErrorCode    int
	ErrorType    string
	ErrorMessage string
}

type PlaceError struct {
	Code      int
	Type      string
	Message   string
	Ambiguous bool
}

func (e *PlaceError) Error() string { return e.Message }

type Client interface {
	Profile(context.Context) (string, error)
	Instruments(context.Context, string) ([]domain.Instrument, error)
	PlaceRegularOrder(context.Context, domain.OrderParams, string) (Order, error)
	Orders(context.Context) ([]Order, error)
	InitiateHoldingsAuthorization(context.Context) (string, error)
}

type Factory interface {
	LoginURL(apiKey, state string) string
	GenerateSession(context.Context, string, string, string) (Session, error)
	Client(apiKey, accessToken string) Client
}
