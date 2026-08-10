package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/abinthomasonline/queueframe/internal/domain"
	ordercheck "github.com/abinthomasonline/queueframe/internal/orders"
	"github.com/abinthomasonline/queueframe/internal/store"
)

var catalogRefreshMu sync.Mutex

func (a *App) traderOrdersPage(w http.ResponseWriter, r *http.Request) {
	trader := traderFrom(r)
	now := time.Now()
	selectedDate := requestedDate(r, a.minimumMutableDate(r, now))
	settings, err := a.store.SettingsForDate(r.Context(), selectedDate)
	if err != nil {
		http.Error(w, "settings for that date are unavailable", http.StatusBadRequest)
		return
	}
	queue, err := a.store.ListTraderOrders(r.Context(), trader.ID, selectedDate)
	if err != nil {
		http.Error(w, "could not load the order queue", http.StatusInternalServerError)
		return
	}
	rows, err := projectOrderRows(queue, settings, now)
	if err != nil {
		http.Error(w, "could not calculate dispatch targets", http.StatusInternalServerError)
		return
	}
	target, _ := dispatchAt(settings, selectedDate)
	freeze := target.Add(-settings.FreezeLead)
	activation, activationFound, err := a.store.TraderDailyActivation(r.Context(), trader.ID, selectedDate)
	if err != nil {
		http.Error(w, "could not load daily activation", http.StatusInternalServerError)
		return
	}
	activationState, err := resolveActivationView(now, queue, settings, activation, activationFound)
	if err != nil {
		http.Error(w, "could not calculate daily activation", http.StatusInternalServerError)
		return
	}
	a.render(w, r, "orders", pageData{
		Title: "Orders", Trader: trader, SelectedDate: selectedDate, Queue: queue, OrderRows: rows,
		QueueRevision: queue.Revision, QueueFrozen: !queue.FrozenAt.IsZero() || !now.Before(freeze),
		DispatchTime: target.In(a.location).Format("15:04:05.000"), FreezeTime: freeze.In(a.location).Format("15:04:05"),
		BatchGapMillis: settings.BatchInterval.Milliseconds(), MaxOrdersPerDate: settings.MaxOrdersPerTrader,
		Activation: activationState, BlackoutStart: formatClock(settings.UserBlackoutStart),
		BlackoutEnd: formatClock(settings.UserBlackoutEnd),
	})
}

func (a *App) traderOrderSchedulePreview(w http.ResponseWriter, r *http.Request) {
	trader := traderFrom(r)
	now := time.Now()
	executionDate := strings.TrimSpace(r.URL.Query().Get("date"))
	if err := ordercheck.ValidateExecutionDate(executionDate, now, a.location); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	settings, err := a.store.SettingsForDate(r.Context(), executionDate)
	if err != nil {
		http.Error(w, "settings for that date are unavailable", http.StatusBadRequest)
		return
	}
	target, err := dispatchAt(settings, executionDate)
	if err != nil {
		http.Error(w, "could not calculate the dispatch schedule", http.StatusInternalServerError)
		return
	}
	freeze := target.Add(-settings.FreezeLead)
	queue, err := a.store.ListTraderOrders(r.Context(), trader.ID, executionDate)
	if err != nil {
		http.Error(w, "could not verify queue mutability", http.StatusInternalServerError)
		return
	}
	if !queue.FrozenAt.IsZero() || !now.Before(freeze) {
		http.Error(w, "the order queue is frozen for that execution date", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"dispatch_time": target.In(a.location).Format("15:04:05.000"),
		"freeze_time":   freeze.In(a.location).Format("15:04:05"),
	})
}

func (a *App) newTraderOrderPage(w http.ResponseWriter, r *http.Request) {
	trader := traderFrom(r)
	now := time.Now()
	minDate := a.minimumMutableDate(r, now)
	selected := requestedDate(r, minDate)
	settings, err := a.store.SettingsForDate(r.Context(), selected)
	if err != nil {
		selected = minDate
		settings, err = a.store.SettingsForDate(r.Context(), selected)
	}
	if err != nil {
		http.Error(w, "runtime settings are unavailable", http.StatusServiceUnavailable)
		return
	}
	target, _ := dispatchAt(settings, selected)
	_ = a.refreshCatalogForTrader(r, trader)
	a.render(w, r, "order_new", pageData{
		Title: "Create order", Trader: trader, SelectedDate: selected, MinExecutionDate: minDate,
		DispatchTime: target.In(a.location).Format("15:04:05.000"), FreezeTime: target.Add(-settings.FreezeLead).In(a.location).Format("15:04:05"),
	})
}

func (a *App) createTraderOrder(w http.ResponseWriter, r *http.Request) {
	trader := traderFrom(r)
	now := time.Now()
	executionDate := strings.TrimSpace(r.Form.Get("execution_date"))
	if err := ordercheck.ValidateExecutionDate(executionDate, now, a.location); err != nil {
		a.redirect(w, r, "/orders/new", "error", err.Error())
		return
	}
	if err := a.refreshCatalogForTrader(r, trader); err != nil {
		a.redirect(w, r, "/orders/new?date="+executionDate, "error", "The Zerodha equity-symbol catalog is unavailable. Complete Kite login and try again.")
		return
	}
	params, err := parseOrderParams(r)
	if err == nil {
		err = a.catalog.Validate(params.Exchange, params.TradingSymbol)
	}
	if err == nil {
		err = ordercheck.ValidateParams(params)
	}
	if err != nil {
		a.redirect(w, r, "/orders/new?date="+executionDate, "error", err.Error())
		return
	}
	created, _, err := a.store.CreateQueuedOrder(r.Context(), trader.ID, domain.QueuedOrder{ExecutionDate: executionDate, Params: params}, now)
	if err != nil {
		a.redirect(w, r, "/orders/new?date="+executionDate, "error", err.Error())
		return
	}
	a.operatorAudit(r, "TRADER", trader.ID, "ORDER_CREATED", "QUEUED_ORDER", created.ID, "trader created scheduled order")
	a.redirect(w, r, "/orders?date="+executionDate, "notice", "Order added at the end of the queue. Reorder it before the preparation cutoff if needed.")
}

func parseOrderParams(r *http.Request) (domain.OrderParams, error) {
	quantity, err := strconv.Atoi(strings.TrimSpace(r.Form.Get("quantity")))
	if err != nil || quantity <= 0 {
		return domain.OrderParams{}, errors.New("quantity must be a positive whole number")
	}
	orderType := strings.ToUpper(strings.TrimSpace(r.Form.Get("order_type")))
	params := domain.OrderParams{
		Exchange:        strings.ToUpper(strings.TrimSpace(r.Form.Get("exchange"))),
		TradingSymbol:   strings.ToUpper(strings.TrimSpace(r.Form.Get("trading_symbol"))),
		TransactionType: strings.ToUpper(strings.TrimSpace(r.Form.Get("transaction_type"))),
		Product:         strings.ToUpper(strings.TrimSpace(r.Form.Get("product"))),
		OrderType:       orderType, Quantity: quantity, Price: strings.TrimSpace(r.Form.Get("price")),
		TriggerPrice: "", Validity: "DAY", MarketProtection: "0",
	}
	if orderType == "MARKET" {
		params.Price = ""
		params.MarketProtection = "-1"
	}
	return ordercheck.Normalize(params), nil
}

func (a *App) reorderTraderOrders(w http.ResponseWriter, r *http.Request) {
	trader := traderFrom(r)
	date := strings.TrimSpace(r.Form.Get("execution_date"))
	if date == "" {
		date = strings.TrimSpace(r.Form.Get("date"))
	}
	if date == "" {
		date = time.Now().In(a.location).Format("2006-01-02")
	}
	revision, ids, err := parseReorderForm(r)
	newRevision := int64(0)
	if err == nil {
		newRevision, err = a.store.ReorderQueuedOrders(r.Context(), trader.ID, date, revision, ids, time.Now())
	}
	if err == nil {
		a.operatorAudit(r, "TRADER", trader.ID, "QUEUE_REORDERED", "TRADER_QUEUE", trader.ID, "trader reordered "+date+" queue")
	}
	a.reorderResponse(w, newRevision, err)
}

func (a *App) deleteTraderOrder(w http.ResponseWriter, r *http.Request) {
	trader := traderFrom(r)
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid order", http.StatusBadRequest)
		return
	}
	order, err := a.store.QueuedOrder(r.Context(), trader.ID, id)
	if err != nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}
	queue, err := a.store.ListTraderOrders(r.Context(), trader.ID, order.ExecutionDate)
	if err == nil {
		_, err = a.store.DeleteQueuedOrder(r.Context(), trader.ID, id, queue.Revision, time.Now())
	}
	if err != nil {
		a.redirect(w, r, "/orders?date="+order.ExecutionDate, "error", err.Error())
		return
	}
	a.operatorAudit(r, "TRADER", trader.ID, "ORDER_DELETED", "QUEUED_ORDER", id, "trader deleted scheduled order")
	a.redirect(w, r, "/orders?date="+order.ExecutionDate, "notice", "Order removed and queue positions recalculated.")
}

func parseReorderForm(r *http.Request) (int64, []int64, error) {
	revision, err := strconv.ParseInt(strings.TrimSpace(r.Form.Get("queue_revision")), 10, 64)
	if err != nil || revision < 0 {
		return 0, nil, errors.New("invalid queue revision")
	}
	values := append([]string(nil), r.Form["order_ids"]...)
	if len(values) == 0 {
		values = append(values, r.Form["order_ids[]"]...)
	}
	if len(values) == 1 && strings.Contains(values[0], ",") {
		values = strings.Split(values[0], ",")
	}
	ids := make([]int64, 0, len(values))
	for _, value := range values {
		id, err := parseID(strings.TrimSpace(value))
		if err != nil {
			return 0, nil, errors.New("invalid order sequence")
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return 0, nil, errors.New("order sequence is empty")
	}
	return revision, ids, nil
}

func (a *App) reorderResponse(w http.ResponseWriter, revision int64, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		status := http.StatusConflict
		if !errors.Is(err, store.ErrQueueFrozen) && !errors.Is(err, store.ErrQueueRevisionConflict) {
			status = http.StatusBadRequest
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "revision": revision})
}

func (a *App) instrumentSearch(w http.ResponseWriter, r *http.Request) {
	trader := traderFrom(r)
	if !a.catalog.IsFresh(time.Now(), a.location) {
		if err := a.refreshCatalogForTrader(r, trader); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "current symbol catalog unavailable"})
			return
		}
	}
	a.writeInstrumentSearch(w, r)
}

func (a *App) adminInstrumentSearch(w http.ResponseWriter, r *http.Request) {
	if !a.catalog.IsFresh(time.Now(), a.location) {
		traderID, err := parseID(r.URL.Query().Get("trader_id"))
		if err != nil {
			http.Error(w, "invalid trader", http.StatusBadRequest)
			return
		}
		trader, err := a.store.TraderByID(r.Context(), traderID)
		if err != nil || a.refreshCatalogForTrader(r, trader) != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "current symbol catalog unavailable"})
			return
		}
	}
	a.writeInstrumentSearch(w, r)
}

func (a *App) writeInstrumentSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	exchange := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("exchange")))
	results := a.catalog.Search(query, 30)
	type item struct {
		Exchange string `json:"exchange"`
		Symbol   string `json:"symbol"`
		Name     string `json:"name"`
	}
	items := make([]item, 0, len(results))
	for _, result := range results {
		if exchange != "" && result.Exchange != exchange {
			continue
		}
		items = append(items, item{Exchange: result.Exchange, Symbol: result.TradingSymbol, Name: result.Name})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

func (a *App) refreshCatalogForTrader(r *http.Request, trader domain.Trader) error {
	if a.catalog.IsFresh(time.Now(), a.location) {
		return nil
	}
	catalogRefreshMu.Lock()
	defer catalogRefreshMu.Unlock()
	if a.catalog.IsFresh(time.Now(), a.location) {
		return nil
	}
	if trader.APIKey == "" || trader.AccessToken == "" || time.Now().After(trader.SessionExpiresAt) {
		return errors.New("a current Kite session is required to load symbols")
	}
	return a.catalog.Refresh(r.Context(), a.broker.Client(trader.APIKey, trader.AccessToken))
}

func requestedDate(r *http.Request, fallback string) string {
	value := strings.TrimSpace(r.URL.Query().Get("date"))
	if _, err := time.Parse("2006-01-02", value); err != nil {
		return fallback
	}
	return value
}

func (a *App) minimumMutableDate(r *http.Request, now time.Time) string {
	local := now.In(a.location)
	date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, a.location)
	for {
		if date.Weekday() != time.Saturday && date.Weekday() != time.Sunday {
			dateText := date.Format("2006-01-02")
			settings, err := a.store.SettingsForDate(r.Context(), dateText)
			if err != nil {
				// Let the caller surface the settings failure instead of spinning
				// indefinitely while SQLite or the settings history is unavailable.
				return dateText
			}
			target, err := dispatchAt(settings, dateText)
			if err != nil {
				return dateText
			}
			if now.Before(target.Add(-settings.FreezeLead)) {
				return dateText
			}
		}
		date = date.AddDate(0, 0, 1)
	}
}
