package broker

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/abinthomasonline/queueframe/internal/domain"
)

// DryRunFactory delegates login, profile, symbol, and authorization calls but
// intercepts PlaceRegularOrder. It is intentionally explicit in configuration
// so a new deployment cannot accidentally submit live orders.
type DryRunFactory struct {
	Factory
	sequence atomic.Uint64
}

func NewDryRunFactory(inner Factory) *DryRunFactory {
	return &DryRunFactory{Factory: inner}
}

func (f *DryRunFactory) Client(apiKey, accessToken string) Client {
	return &dryRunClient{Client: f.Factory.Client(apiKey, accessToken), factory: f}
}

type dryRunClient struct {
	Client
	factory *DryRunFactory
}

func (c *dryRunClient) PlaceRegularOrder(ctx context.Context, params domain.OrderParams, tag string) (Order, error) {
	if err := ctx.Err(); err != nil {
		return Order{}, err
	}
	id := c.factory.sequence.Add(1)
	return Order{OrderID: fmt.Sprintf("DRY%010d", id), Status: "DRY_RUN", Tag: tag, Params: params}, nil
}
