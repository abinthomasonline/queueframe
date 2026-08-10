package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/abinthomasonline/queueframe/internal/domain"
)

func (a *App) traderDispatchesPage(w http.ResponseWriter, r *http.Request) {
	trader := traderFrom(r)
	date := requestedDate(r, time.Now().In(a.location).Format("2006-01-02"))
	records, err := a.store.ListTraderDispatches(r.Context(), trader.ID, date)
	if err != nil {
		http.Error(w, "could not load dispatch history", http.StatusInternalServerError)
		return
	}
	status := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status")))
	symbol := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	rows := filterDispatchRows(decodeDispatchRows(records), status, symbol, "")
	a.render(w, r, "dispatches", pageData{
		Title: "Dispatches", Trader: trader, SelectedDate: date, SelectedStatus: status, SelectedSymbol: symbol,
		DispatchRows: rows, DispatchSummary: summarizeDispatches(rows), DispatchStatuses: dispatchStatusOptions(status),
	})
}

func (a *App) adminDispatchesPageV2(w http.ResponseWriter, r *http.Request) {
	date := requestedDate(r, time.Now().In(a.location).Format("2006-01-02"))
	records, err := a.store.ListAllDispatches(r.Context(), date)
	if err != nil {
		http.Error(w, "could not load dispatch history", http.StatusInternalServerError)
		return
	}
	status := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status")))
	clientID := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("client_id")))
	rows := filterDispatchRows(decodeDispatchRows(records), status, "", clientID)
	a.render(w, r, "admin_dispatches", pageData{
		Title: "All dispatches", Admin: adminFrom(r), SelectedDate: date, SelectedStatus: status,
		SelectedClientID: clientID, DispatchRows: rows, DispatchSummary: summarizeDispatches(rows),
		DispatchStatuses: dispatchStatusOptions(status),
	})
}

func filterDispatchRows(rows []dispatchRow, status, symbol, clientID string) []dispatchRow {
	filtered := rows[:0]
	for _, row := range rows {
		if status != "" && row.Result.Outcome != status {
			continue
		}
		if symbol != "" && !strings.Contains(strings.ToUpper(row.Params.TradingSymbol), symbol) {
			continue
		}
		if clientID != "" && !strings.Contains(strings.ToUpper(row.ClientID), clientID) {
			continue
		}
		filtered = append(filtered, row)
	}
	return filtered
}

func dispatchStatusOptions(selected string) []selectOption {
	states := []string{domain.OrderStatePrepared, domain.OrderStateAcknowledged, domain.OrderStateComplete,
		domain.OrderStateRejected, domain.OrderStateCancelled, domain.OrderStateUnknown,
		domain.OrderStateMissed, domain.OrderStateMissedInactive}
	options := make([]selectOption, 0, len(states))
	for _, state := range states {
		options = append(options, selectOption{Value: state, Label: humanState(state), Selected: state == selected})
	}
	return options
}

func humanState(state string) string {
	words := strings.Fields(strings.ReplaceAll(strings.ToLower(state), "_", " "))
	for index := range words {
		if len(words[index]) > 0 {
			words[index] = strings.ToUpper(words[index][:1]) + words[index][1:]
		}
	}
	return strings.Join(words, " ")
}
