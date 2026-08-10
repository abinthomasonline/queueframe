package web

import (
	"strings"
	"testing"
	"time"

	"github.com/abinthomasonline/queueframe/internal/domain"
)

func activationTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", value, india)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func activationTestSettings() domain.RuntimeSettings {
	return domain.RuntimeSettings{
		DispatchClock: "09:00:00", UserBlackoutStart: "08:45:00", UserBlackoutEnd: "09:30:00",
		FreezeLead: 5 * time.Minute, BatchInterval: 1050 * time.Millisecond, MaxOrdersPerTrader: 100,
	}
}

func activationTestQueue(date, state string) domain.OrderQueue {
	return domain.OrderQueue{ExecutionDate: date, Orders: []domain.QueuedOrder{{
		ID: 1, ExecutionDate: date, Position: 1, State: state,
	}}}
}

func TestResolveActivationViewDateAndQueueScope(t *testing.T) {
	settings := activationTestSettings()
	tests := []struct {
		name       string
		now        string
		queue      domain.OrderQueue
		visible    bool
		title      string
		showAction bool
	}{
		{name: "empty queue", now: "2026-08-10 07:00:00", queue: domain.OrderQueue{ExecutionDate: "2026-08-10"}},
		{name: "past queue", now: "2026-08-11 07:00:00", queue: activationTestQueue("2026-08-10", domain.OrderStateScheduled)},
		{name: "weekend queue", now: "2026-08-09 07:00:00", queue: activationTestQueue("2026-08-09", domain.OrderStateScheduled)},
		{name: "future weekend queue", now: "2026-08-09 10:55:00", queue: activationTestQueue("2026-08-15", domain.OrderStateScheduled)},
		{name: "future queue", now: "2026-08-09 10:55:00", queue: activationTestQueue("2026-08-10", domain.OrderStateScheduled), visible: true, title: "Activation required on 10 Aug 2026"},
		{name: "future terminal queue", now: "2026-08-09 10:55:00", queue: activationTestQueue("2026-08-10", domain.OrderStateComplete)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			view, err := resolveActivationView(activationTestTime(t, test.now), test.queue, settings, domain.DailyActivation{}, false)
			if err != nil {
				t.Fatal(err)
			}
			if view.Visible != test.visible || view.Title != test.title || view.ShowAction != test.showAction {
				t.Fatalf("view = %#v", view)
			}
		})
	}
}

func TestResolveActivationViewBoundaries(t *testing.T) {
	settings := activationTestSettings()
	queue := activationTestQueue("2026-08-10", domain.OrderStateScheduled)
	tests := []struct {
		name       string
		now        string
		title      string
		showAction bool
		refreshAt  string
	}{
		{name: "before six", now: "2026-08-10 05:59:59", title: "Activation opens at 06:00 IST", refreshAt: "2026-08-10 06:00:00"},
		{name: "at six", now: "2026-08-10 06:00:00", title: "Activate today's orders", showAction: true, refreshAt: "2026-08-10 08:45:00"},
		{name: "before close", now: "2026-08-10 08:44:59", title: "Activate today's orders", showAction: true, refreshAt: "2026-08-10 08:45:00"},
		{name: "at close", now: "2026-08-10 08:45:00", title: "Today's activation window has closed"},
		{name: "blackout end", now: "2026-08-10 09:30:00", title: "Today's activation window has closed"},
		{name: "reported weekday time", now: "2026-08-10 10:55:00", title: "Today's activation window has closed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			view, err := resolveActivationView(activationTestTime(t, test.now), queue, settings, domain.DailyActivation{}, false)
			if err != nil {
				t.Fatal(err)
			}
			if view.Title != test.title || view.ShowAction != test.showAction {
				t.Fatalf("view = %#v", view)
			}
			if test.refreshAt == "" {
				if !view.RefreshAt.IsZero() {
					t.Fatalf("RefreshAt = %v", view.RefreshAt)
				}
			} else if want := activationTestTime(t, test.refreshAt); !view.RefreshAt.Equal(want) {
				t.Fatalf("RefreshAt = %v, want %v", view.RefreshAt, want)
			}
		})
	}
}

func TestResolveActivationViewActiveAndMissed(t *testing.T) {
	settings := activationTestSettings()
	queue := activationTestQueue("2026-08-10", domain.OrderStateScheduled)
	activation := domain.DailyActivation{
		TraderID: 1, ExecutionDate: "2026-08-10", TokenPresent: true,
		LoginAt: activationTestTime(t, "2026-08-10 07:00:00"), ExpiresAt: activationTestTime(t, "2026-08-11 06:00:00"),
	}
	active, err := resolveActivationView(activationTestTime(t, "2026-08-10 08:00:00"), queue, settings, activation, true)
	if err != nil || active.Title != "Today's orders are activated" || active.ShowAction || active.CSSClass != "activation-active" {
		t.Fatalf("active view = %#v, %v", active, err)
	}
	historical, err := resolveActivationView(activationTestTime(t, "2026-08-10 10:55:00"), queue, settings, activation, true)
	if err != nil || historical.Title != "Today's activation was recorded" || historical.ShowAction {
		t.Fatalf("historical view = %#v, %v", historical, err)
	}
	missedQueue := activationTestQueue("2026-08-10", domain.OrderStateMissedInactive)
	missed, err := resolveActivationView(activationTestTime(t, "2026-08-10 10:55:00"), missedQueue, settings, domain.DailyActivation{}, false)
	if err != nil || missed.Title != "Orders were not dispatched" || !strings.Contains(missed.Message, "activation was missing") || missed.ShowAction {
		t.Fatalf("missed view = %#v, %v", missed, err)
	}
}

func TestResolveKiteAuthSuccessNotice(t *testing.T) {
	openAt := activationTestTime(t, "2026-08-10 06:00:00")
	closeAt := activationTestTime(t, "2026-08-10 08:45:00")
	tests := []struct {
		name          string
		completedAt   string
		loginAt       string
		weekday       bool
		activated     bool
		hasScheduled  bool
		wantSubstring string
	}{
		{name: "weekend", completedAt: "2026-08-09 10:55:00", loginAt: "2026-08-09 10:54:58", wantSubstring: "does not dispatch weekend orders"},
		{name: "before six", completedAt: "2026-08-10 05:59:30", loginAt: "2026-08-10 05:59:00", weekday: true, wantSubstring: "sign in again between 06:00 and 08:45:00"},
		{name: "activated with orders", completedAt: "2026-08-10 07:00:02", loginAt: "2026-08-10 07:00:00", weekday: true, activated: true, hasScheduled: true, wantSubstring: "scheduled orders are activated"},
		{name: "activated without orders", completedAt: "2026-08-10 07:00:02", loginAt: "2026-08-10 07:00:00", weekday: true, activated: true, wantSubstring: "no orders are currently scheduled"},
		{name: "post dispatch access only", completedAt: "2026-08-10 10:55:00", loginAt: "2026-08-10 10:54:58", weekday: true, wantSubstring: "provides Queueframe access but does not activate"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := resolveKiteAuthSuccessNotice(
				activationTestTime(t, test.completedAt), activationTestTime(t, test.loginAt),
				openAt, closeAt, test.weekday, test.activated, test.hasScheduled,
			)
			if !strings.Contains(got, test.wantSubstring) {
				t.Fatalf("notice = %q, want substring %q", got, test.wantSubstring)
			}
		})
	}
}
