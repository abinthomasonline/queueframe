package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/abinthomasonline/queueframe/internal/domain"
	ordercheck "github.com/abinthomasonline/queueframe/internal/orders"
	"github.com/abinthomasonline/queueframe/internal/store"
)

func (a *App) adminOrdersPageV2(w http.ResponseWriter, r *http.Request) {
	admin := adminFrom(r)
	now := time.Now()
	date := requestedDate(r, now.In(a.location).Format("2006-01-02"))
	selectedClient := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("client_id")))
	selectedState := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("state")))
	traders, err := a.store.ListTraders(r.Context(), false)
	if err != nil {
		http.Error(w, "could not load traders", http.StatusInternalServerError)
		return
	}
	settings, err := a.store.SettingsForDate(r.Context(), date)
	if err != nil {
		http.Error(w, "settings for that date are unavailable", http.StatusBadRequest)
		return
	}
	target, _ := dispatchAt(settings, date)
	freeze := target.Add(-settings.FreezeLead)
	queues := make([]adminQueue, 0, len(traders))
	for _, trader := range traders {
		if selectedClient != "" && !strings.Contains(strings.ToUpper(trader.DisplayClientID), selectedClient) {
			continue
		}
		queue, err := a.store.ListTraderOrders(r.Context(), trader.ID, date)
		if err != nil || len(queue.Orders) == 0 {
			continue
		}
		rows, err := projectOrderRows(queue, settings, now)
		if err != nil {
			continue
		}
		if selectedState != "" {
			filtered := rows[:0]
			for _, row := range rows {
				if row.State == selectedState {
					filtered = append(filtered, row)
				}
			}
			rows = filtered
		}
		if len(rows) == 0 {
			continue
		}
		queues = append(queues, adminQueue{
			TraderID: trader.ID, ClientID: trader.DisplayClientID, ExecutionDate: date,
			Revision: queue.Revision, Frozen: !queue.FrozenAt.IsZero() || !now.Before(freeze),
			FreezeTime: freeze.In(a.location).Format("15:04:05"), DispatchTime: target.In(a.location).Format("15:04:05.000"),
			BatchGapMillis: settings.BatchInterval.Milliseconds(), OrderRows: rows,
		})
	}
	a.render(w, r, "admin_orders", pageData{
		Title: "All orders", Admin: admin, SelectedDate: date, SelectedClientID: selectedClient,
		AdminQueues: queues, OrderStates: stateOptions(selectedState),
	})
}

func (a *App) reorderAdminOrders(w http.ResponseWriter, r *http.Request) {
	traderID, err := parseID(r.Form.Get("trader_id"))
	if err != nil {
		a.reorderResponse(w, 0, errors.New("invalid trader"))
		return
	}
	date := strings.TrimSpace(r.Form.Get("execution_date"))
	revision, ids, err := parseReorderForm(r)
	newRevision := int64(0)
	if err == nil {
		newRevision, err = a.store.ReorderQueuedOrders(r.Context(), traderID, date, revision, ids, time.Now())
	}
	if err == nil {
		a.operatorAudit(r, "ADMIN", adminFrom(r).ID, "QUEUE_REORDERED", "TRADER_QUEUE", traderID, "administrator reordered "+date+" queue")
	}
	a.reorderResponse(w, newRevision, err)
}

func (a *App) deleteAdminOrder(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid order", http.StatusBadRequest)
		return
	}
	order, err := a.store.QueuedOrderByID(r.Context(), id)
	if err != nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}
	queue, err := a.store.ListTraderOrders(r.Context(), order.TraderID, order.ExecutionDate)
	if err == nil {
		_, err = a.store.DeleteQueuedOrder(r.Context(), order.TraderID, id, queue.Revision, time.Now())
	}
	if err != nil {
		a.redirect(w, r, "/admin/orders?date="+order.ExecutionDate, "error", err.Error())
		return
	}
	a.operatorAudit(r, "ADMIN", adminFrom(r).ID, "ORDER_DELETED", "QUEUED_ORDER", id, "administrator deleted queued order")
	a.redirect(w, r, "/admin/orders?date="+order.ExecutionDate, "notice", "Order deleted and queue positions recalculated.")
}

func (a *App) adminOrderEditPage(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid order", http.StatusBadRequest)
		return
	}
	order, err := a.store.QueuedOrderByID(r.Context(), id)
	if err != nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}
	trader, err := a.store.TraderByID(r.Context(), order.TraderID)
	if err != nil {
		http.Error(w, "trader not found", http.StatusNotFound)
		return
	}
	queue, err := a.store.ListTraderOrders(r.Context(), order.TraderID, order.ExecutionDate)
	if err != nil {
		http.Error(w, "could not load queue", http.StatusInternalServerError)
		return
	}
	_ = a.refreshCatalogForTrader(r, trader)
	a.render(w, r, "admin_order_edit", pageData{
		Title: "Edit order", Admin: adminFrom(r), EditOrder: order, EditTrader: trader,
		QueueRevision: queue.Revision, FormAction: "/admin/orders/" + strconv.FormatInt(id, 10) + "/edit",
		MinExecutionDate: a.minimumMutableDate(r, time.Now()),
	})
}

func (a *App) updateAdminOrder(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid order", http.StatusBadRequest)
		return
	}
	current, err := a.store.QueuedOrderByID(r.Context(), id)
	if err != nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}
	expectedSourceDate := strings.TrimSpace(r.Form.Get("original_execution_date"))
	if expectedSourceDate != current.ExecutionDate {
		a.redirect(w, r, "/admin/orders/"+strconv.FormatInt(id, 10)+"/edit", "error", store.ErrQueueRevisionConflict.Error())
		return
	}
	revision, err := strconv.ParseInt(strings.TrimSpace(r.Form.Get("queue_revision")), 10, 64)
	if err != nil || revision < 0 {
		a.redirect(w, r, "/admin/orders/"+strconv.FormatInt(id, 10)+"/edit", "error", "Invalid queue revision; reload the order.")
		return
	}
	executionDate := strings.TrimSpace(r.Form.Get("execution_date"))
	if err := ordercheck.ValidateExecutionDate(executionDate, time.Now(), a.location); err != nil {
		a.redirect(w, r, "/admin/orders/"+strconv.FormatInt(id, 10)+"/edit", "error", err.Error())
		return
	}
	trader, err := a.store.TraderByID(r.Context(), current.TraderID)
	if err != nil {
		a.redirect(w, r, "/admin/orders/"+strconv.FormatInt(id, 10)+"/edit", "error", "The trader profile is unavailable.")
		return
	}
	params, err := parseOrderParams(r)
	if err == nil && (params.Exchange != current.Params.Exchange || params.TradingSymbol != current.Params.TradingSymbol) {
		if refreshErr := a.refreshCatalogForTrader(r, trader); refreshErr != nil {
			err = errors.New("the current Zerodha equity-symbol catalog is unavailable")
		} else {
			err = a.catalog.Validate(params.Exchange, params.TradingSymbol)
		}
	}
	if err == nil {
		err = ordercheck.ValidateParams(params)
	}
	if err != nil {
		a.redirect(w, r, "/admin/orders/"+strconv.FormatInt(id, 10)+"/edit", "error", err.Error())
		return
	}
	updated, _, err := a.store.UpdateQueuedOrder(r.Context(), current.TraderID, id, expectedSourceDate, revision,
		domain.QueuedOrder{ExecutionDate: executionDate, Params: params}, time.Now())
	if err != nil {
		a.redirect(w, r, "/admin/orders/"+strconv.FormatInt(id, 10)+"/edit", "error", err.Error())
		return
	}
	a.operatorAudit(r, "ADMIN", adminFrom(r).ID, "ORDER_UPDATED", "QUEUED_ORDER", id, "administrator modified queued order")
	a.redirect(w, r, "/admin/orders?date="+updated.ExecutionDate, "notice", "Order changes saved and dispatch timing recalculated.")
}

func stateOptions(selected string) []selectOption {
	states := []string{domain.OrderStateScheduled, domain.OrderStatePrepared, domain.OrderStateAcknowledged,
		domain.OrderStateComplete, domain.OrderStateRejected, domain.OrderStateCancelled,
		domain.OrderStateUnknown, domain.OrderStateMissed, domain.OrderStateMissedInactive}
	options := make([]selectOption, 0, len(states))
	for _, state := range states {
		options = append(options, selectOption{Value: state, Label: humanState(state), Selected: state == selected})
	}
	return options
}

func (a *App) adminUsersPage(w http.ResponseWriter, r *http.Request) {
	admin := adminFrom(r)
	now := time.Now()
	today := now.In(a.location).Format("2006-01-02")
	settings, _ := a.store.SettingsForDate(r.Context(), today)
	target, _ := dispatchAt(settings, today)
	query := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("q")))
	traders, err := a.store.ListTraders(r.Context(), false)
	if err != nil {
		http.Error(w, "could not load traders", http.StatusInternalServerError)
		return
	}
	activationStatuses, err := a.store.DailyActivationStatuses(r.Context(), today, target)
	if err != nil {
		http.Error(w, "could not load daily activations", http.StatusInternalServerError)
		return
	}
	orders, _ := a.store.ListAllQueuedOrders(r.Context(), "")
	frozenByDate := make(map[string]bool)
	rows := make([]traderRow, 0, len(traders))
	localNow := now.In(a.location)
	activationApplicable := localNow.Weekday() != time.Saturday && localNow.Weekday() != time.Sunday
	for _, trader := range traders {
		if query != "" && !strings.Contains(strings.ToUpper(trader.DisplayClientID), query) {
			continue
		}
		row := traderRow{
			Trader: trader, ActivationApplicable: activationApplicable,
			ActiveToday: activationStatuses[trader.ID], CanDelete: true,
		}
		for _, order := range orders {
			if order.TraderID != trader.ID {
				continue
			}
			if order.State == domain.OrderStateScheduled {
				row.QueuedCount++
				frozen, known := frozenByDate[order.ExecutionDate]
				if !known {
					dateSettings, settingsErr := a.store.SettingsForDate(r.Context(), order.ExecutionDate)
					dateTarget, targetErr := dispatchAt(dateSettings, order.ExecutionDate)
					frozen = settingsErr != nil || targetErr != nil || !now.Before(dateTarget.Add(-dateSettings.FreezeLead))
					frozenByDate[order.ExecutionDate] = frozen
				}
				if frozen {
					row.CanDelete = false
				}
			}
			if order.State == domain.OrderStatePrepared || order.State == domain.OrderStateDispatching {
				row.NonterminalCount++
				row.CanDelete = false
			} else if order.State == domain.OrderStateUnknown {
				row.NonterminalCount++
			}
		}
		rows = append(rows, row)
	}
	a.render(w, r, "users", pageData{Title: "Users", Admin: admin, TraderRows: rows, SearchQuery: query})
}

func (a *App) adminDashboardV2(w http.ResponseWriter, r *http.Request) {
	admin := adminFrom(r)
	now := time.Now()
	today := now.In(a.location).Format("2006-01-02")
	currentSettings, err := a.store.SettingsForDate(r.Context(), today)
	if err != nil {
		http.Error(w, "runtime settings are unavailable", http.StatusServiceUnavailable)
		return
	}
	todayTarget, _ := dispatchAt(currentSettings, today)
	traders, _ := a.store.ListTraders(r.Context(), false)
	activationStatuses, err := a.store.DailyActivationStatuses(r.Context(), today, todayTarget)
	if err != nil {
		http.Error(w, "could not load daily activations", http.StatusInternalServerError)
		return
	}
	orders, _ := a.store.ListAllQueuedOrders(r.Context(), today)
	records, _ := a.store.ListAllDispatches(r.Context(), today)
	rows := decodeDispatchRows(records)
	stats := adminStats{TraderCount: len(traders), DispatchTotalToday: len(records)}
	for _, trader := range traders {
		if activationStatuses[trader.ID] {
			stats.ActiveToday++
		}
	}
	for _, order := range orders {
		if order.State == domain.OrderStateScheduled {
			stats.QueuedToday++
		}
		if order.State == domain.OrderStatePrepared {
			stats.PreparedToday++
		}
	}
	stats.NeedsAttention = summarizeDispatches(rows).NeedsAttention
	if len(rows) > 8 {
		rows = rows[:8]
	}
	nextDate := today
	localNow := now.In(a.location)
	if localNow.Weekday() == time.Saturday || localNow.Weekday() == time.Sunday || !now.Before(todayTarget) {
		nextDate = nextWeekdayDate(now.In(a.location)).Format("2006-01-02")
	}
	nextSettings, err := a.store.SettingsForDate(r.Context(), nextDate)
	if err != nil {
		http.Error(w, "next runtime settings are unavailable", http.StatusServiceUnavailable)
		return
	}
	nextTarget, _ := dispatchAt(nextSettings, nextDate)
	a.render(w, r, "admin_dashboard", pageData{
		Title: "Overview", Admin: admin, Settings: nextSettings, AdminStats: stats,
		NextDispatchDate: nextDate, DispatchTime: nextTarget.In(a.location).Format("15:04:05.000"),
		FreezeTime:     nextTarget.Add(-nextSettings.FreezeLead).In(a.location).Format("15:04:05"),
		BlackoutActive: store.UserLoginBlocked(currentSettings, now), BlackoutEnd: formatClock(currentSettings.UserBlackoutEnd),
		RedirectURL: a.config.RedirectURL, RecentDispatchRows: rows,
	})
}

func (a *App) operatorAudit(r *http.Request, actorType string, actorID int64, eventType, subjectType string, subjectID int64, detail string) {
	if err := a.store.WriteOperatorAudit(r.Context(), actorType, actorID, eventType, subjectType, subjectID, detail, time.Now()); err != nil {
		slog.Error("write operator audit", "actor_type", actorType, "actor_id", actorID, "event", eventType, "subject_type", subjectType, "subject_id", subjectID, "error", err)
	}
}
