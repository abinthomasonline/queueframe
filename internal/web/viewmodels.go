package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/abinthomasonline/queueframe/internal/domain"
	"github.com/abinthomasonline/queueframe/internal/store"
)

var india = time.FixedZone("Asia/Kolkata", 5*60*60+30*60)

type selectOption struct {
	Value    string
	Label    string
	Selected bool
}

// activationView is the single presentation state for date-bound Kite
// activation. Templates render this resolved value and never infer timing from
// independent booleans such as "blackout active" and "active today".
type activationView struct {
	Visible     bool
	CSSClass    string
	Mark        string
	Title       string
	Message     string
	ShowAction  bool
	ActionLabel string
	RefreshAt   time.Time
}

func resolveActivationView(now time.Time, queue domain.OrderQueue, settings domain.RuntimeSettings, activation domain.DailyActivation, activationFound bool) (activationView, error) {
	if queue.ExecutionDate == "" {
		return activationView{}, nil
	}
	date, err := time.ParseInLocation("2006-01-02", queue.ExecutionDate, india)
	if err != nil {
		return activationView{}, err
	}
	localNow := now.In(india)
	today := localNow.Format("2006-01-02")
	hasScheduled := false
	hasPending := false
	hasMissedInactive := false
	maxPosition := 1
	for _, order := range queue.Orders {
		switch order.State {
		case domain.OrderStateScheduled:
			hasScheduled = true
			hasPending = true
			if order.Position > maxPosition {
				maxPosition = order.Position
			}
		case domain.OrderStatePrepared, domain.OrderStateDispatching:
			hasPending = true
			if order.Position > maxPosition {
				maxPosition = order.Position
			}
		case domain.OrderStateMissedInactive:
			hasMissedInactive = true
		}
	}
	if len(queue.Orders) == 0 {
		return activationView{}, nil
	}
	if date.Weekday() == time.Saturday || date.Weekday() == time.Sunday {
		return activationView{}, nil
	}
	if queue.ExecutionDate < today {
		return activationView{}, nil
	}
	openAt, closeAt, err := store.DailyActivationWindow(settings, queue.ExecutionDate)
	if err != nil {
		return activationView{}, err
	}
	dateLabel := date.Format("02 Jan 2006")
	if queue.ExecutionDate > today {
		if !hasScheduled {
			return activationView{}, nil
		}
		return activationView{
			Visible: true, CSSClass: "activation-inactive", Mark: "i",
			Title:     "Activation required on " + dateLabel,
			Message:   "On " + dateLabel + ", complete Kite login between 06:00 and " + closeAt.In(india).Format("15:04:05") + " IST. Activation cannot be completed in advance.",
			RefreshAt: openAt,
		}, nil
	}
	if !hasPending {
		if hasMissedInactive {
			return activationView{
				Visible: true, CSSClass: "activation-inactive", Mark: "!",
				Title:   "Orders were not dispatched",
				Message: "The same-day Kite activation was missing when Queueframe prepared this queue. Review Dispatches for the recorded outcome.",
			}, nil
		}
		return activationView{}, nil
	}
	baseTarget, err := dispatchAt(settings, queue.ExecutionDate)
	if err != nil {
		return activationView{}, err
	}
	lastTarget := baseTarget.Add(time.Duration((maxPosition-1)/10) * settings.BatchInterval)
	if activationFound && activation.ActiveFor(queue.ExecutionDate, lastTarget) {
		if now.Before(lastTarget) {
			return activationView{
				Visible: true, CSSClass: "activation-active", Mark: "✓",
				Title:     "Today's orders are activated",
				Message:   "Kite login was recorded at " + activation.LoginAt.In(india).Format("02 Jan 2006 15:04:05") + " IST. Eligible orders can proceed at their planned request times.",
				RefreshAt: lastTarget,
			}, nil
		}
		return activationView{
			Visible: true, CSSClass: "activation-active", Mark: "✓",
			Title:   "Today's activation was recorded",
			Message: "Kite login was completed before the activation deadline. Review Dispatches for the recorded order outcomes.",
		}, nil
	}
	if localNow.Before(openAt) {
		return activationView{
			Visible: true, CSSClass: "activation-inactive", Mark: "i",
			Title:     "Activation opens at 06:00 IST",
			Message:   "Complete Kite login between 06:00 and " + closeAt.In(india).Format("15:04:05") + " IST today to activate these orders.",
			RefreshAt: openAt,
		}, nil
	}
	if localNow.Before(closeAt) {
		return activationView{
			Visible: true, CSSClass: "activation-inactive", Mark: "!",
			Title:      "Activate today's orders",
			Message:    "Complete Kite login before " + closeAt.In(india).Format("15:04:05") + " IST. If verification does not finish before then, these orders will not be sent.",
			ShowAction: true, ActionLabel: "Activate with Kite", RefreshAt: closeAt,
		}, nil
	}
	return activationView{
		Visible: true, CSSClass: "activation-inactive", Mark: "!",
		Title:   "Today's activation window has closed",
		Message: "Kite login was not completed between 06:00 and " + closeAt.In(india).Format("15:04:05") + " IST. Today's unsent orders cannot be activated now.",
	}, nil
}

// orderRow is deliberately a projection. Scheduling information is derived
// from the immutable settings version for the order's date, while the stored
// queue position remains the source of truth.
type orderRow struct {
	domain.QueuedOrder
	PlannedAt   time.Time
	PlannedTime string
	Batch       int
	StartsBatch bool
	Mutable     bool
}

type dispatchRow struct {
	domain.DispatchRecord
	Params domain.OrderParams
}

type dispatchSummary struct {
	Total           int
	Acknowledged    int
	Rejected        int
	Unknown         int
	Missed          int
	UnknownOrMissed int
	NeedsAttention  int
}

type traderRow struct {
	Trader               domain.Trader
	ActivationApplicable bool
	ActiveToday          bool
	QueuedCount          int
	NonterminalCount     int
	CanDelete            bool
}

type adminQueue struct {
	TraderID       int64
	ClientID       string
	ExecutionDate  string
	Revision       int64
	Frozen         bool
	FreezeTime     string
	DispatchTime   string
	BatchGapMillis int64
	OrderRows      []orderRow
}

type adminStats struct {
	TraderCount        int
	ActiveToday        int
	QueuedToday        int
	PreparedToday      int
	DispatchTotalToday int
	NeedsAttention     int
}

type signupInvitationRow struct {
	domain.SignupInvitation
	Status    string
	CanRevoke bool
}

func projectSignupInvitations(invitations []domain.SignupInvitation, now time.Time) []signupInvitationRow {
	rows := make([]signupInvitationRow, 0, len(invitations))
	for _, invitation := range invitations {
		row := signupInvitationRow{SignupInvitation: invitation}
		switch {
		case !invitation.ConsumedAt.IsZero():
			row.Status = "Used"
		case !invitation.RevokedAt.IsZero():
			row.Status = "Revoked"
		case !invitation.ExpiresAt.After(now):
			row.Status = "Expired"
		case invitation.ReservedUntil.After(now):
			row.Status = "In verification"
			row.CanRevoke = true
		default:
			row.Status = "Available"
			row.CanRevoke = true
		}
		rows = append(rows, row)
	}
	return rows
}

func projectOrderRows(queue domain.OrderQueue, settings domain.RuntimeSettings, now time.Time) ([]orderRow, error) {
	target, err := dispatchAt(settings, queue.ExecutionDate)
	if err != nil {
		return nil, err
	}
	freezeAt := target.Add(-settings.FreezeLead)
	rows := make([]orderRow, 0, len(queue.Orders))
	for _, order := range queue.Orders {
		batch := batchForPosition(order.Position)
		planned := target.Add(time.Duration(batch-1) * settings.BatchInterval)
		rows = append(rows, orderRow{
			QueuedOrder: order,
			PlannedAt:   planned,
			PlannedTime: planned.In(india).Format("15:04:05.000"),
			Batch:       batch,
			StartsBatch: order.Position == 1 || (order.Position-1)%10 == 0,
			Mutable:     order.State == domain.OrderStateScheduled && order.DeletedAt.IsZero() && now.Before(freezeAt) && queue.FrozenAt.IsZero(),
		})
	}
	return rows, nil
}

func batchForPosition(position int) int {
	if position <= 0 {
		return 0
	}
	return (position-1)/10 + 1
}

func decodeDispatchRows(records []domain.DispatchRecord) []dispatchRow {
	rows := make([]dispatchRow, 0, len(records))
	for _, record := range records {
		var params domain.OrderParams
		if err := json.Unmarshal([]byte(record.PayloadJSON), &params); err != nil || params.TradingSymbol == "" {
			var wrapped struct {
				Params domain.OrderParams `json:"params"`
			}
			_ = json.Unmarshal([]byte(record.PayloadJSON), &wrapped)
			params = wrapped.Params
		}
		rows = append(rows, dispatchRow{DispatchRecord: record, Params: params})
	}
	return rows
}

func summarizeDispatches(rows []dispatchRow) dispatchSummary {
	var summary dispatchSummary
	for _, row := range rows {
		summary.Total++
		switch row.Result.Outcome {
		case domain.OrderStateAcknowledged, domain.OrderStateComplete:
			summary.Acknowledged++
		case domain.OrderStateRejected, domain.OrderStateCancelled:
			summary.Rejected++
		case domain.OrderStateUnknown:
			summary.Unknown++
			summary.UnknownOrMissed++
		case domain.OrderStateMissed, domain.OrderStateMissedInactive:
			summary.Missed++
			summary.UnknownOrMissed++
		}
		if row.Result.Outcome == domain.OrderStateUnknown || row.Result.Outcome == domain.OrderStateRejected || row.Result.Outcome == domain.OrderStateCancelled ||
			row.Result.Outcome == domain.OrderStateMissed || row.Result.Outcome == domain.OrderStateMissedInactive {
			summary.NeedsAttention++
		}
	}
	return summary
}

func normalizeHTMLClock(value string) string {
	value = strings.TrimSpace(value)
	if len(value) == 5 {
		return value + ":00"
	}
	return value
}

func dispatchAt(settings domain.RuntimeSettings, executionDate string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02 15:04:05", executionDate+" "+settings.DispatchClock, india)
}

func formatClock(value string) string {
	parsed, err := time.Parse("15:04:05", value)
	if err != nil {
		return value
	}
	return parsed.Format("15:04:05")
}

func durationMinutes(value time.Duration) string {
	if value%time.Minute == 0 {
		return fmt.Sprintf("%d", value/time.Minute)
	}
	return fmt.Sprintf("%.2f", value.Minutes())
}
