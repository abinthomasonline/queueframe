package web

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/abinthomasonline/queueframe/internal/domain"
)

func TestOrderFormsShareAccessibleConsistentControls(t *testing.T) {
	app, database := newTestApp(t)
	defer database.Close()

	create := executeOrderFormTemplate(t, app, "order_new", pageData{
		Now: time.Now(), Trader: domain.Trader{ID: 1, DisplayClientID: "AB1234"},
		SelectedDate: "2026-08-10", MinExecutionDate: "2026-08-10", DispatchTime: "09:00:00.000", FreezeTime: "08:55:00",
	})
	adminLimit := executeOrderFormTemplate(t, app, "admin_order_edit", adminOrderFormData("LIMIT"))
	adminMarket := executeOrderFormTemplate(t, app, "admin_order_edit", adminOrderFormData("MARKET"))

	shared := []string{
		`class="order-detail-stack"`,
		`Submit only at your stated limit price or better.`,
		`Submit at the available market; execution price is not guaranteed.`,
		`data-empty-hint="Select a result; free-form symbols are not accepted."`,
		`aria-live="polite" aria-atomic="true"`,
		`role="alert" hidden>Please select a symbol from the results.`,
		`placeholder="Search by symbol or company"`,
		`autocapitalize="characters" spellcheck="false"`,
	}
	for _, body := range []string{create, adminLimit} {
		for _, fragment := range shared {
			if !strings.Contains(body, fragment) {
				t.Fatalf("order form is missing shared UI fragment %q", fragment)
			}
		}
	}
	if !strings.Contains(create, `data-schedule-preview-url="/orders/schedule-preview"`) {
		t.Fatal("create form must refresh its date-dependent schedule preview")
	}

	pricePattern := regexp.MustCompile(`(?s)<div class="field price-field" data-limit-price-field([^>]*)>.*?<input id="edit-limit-price"([^>]*)>`)
	limitMatch := pricePattern.FindStringSubmatch(adminLimit)
	marketMatch := pricePattern.FindStringSubmatch(adminMarket)
	if len(limitMatch) != 3 || strings.Contains(limitMatch[1], "hidden") || !strings.Contains(limitMatch[2], "required") || strings.Contains(limitMatch[2], "disabled") {
		t.Fatalf("LIMIT edit must initially show and require price: %#v", limitMatch)
	}
	if len(marketMatch) != 3 || !strings.Contains(marketMatch[1], "hidden") || !strings.Contains(marketMatch[2], "disabled") || strings.Contains(marketMatch[2], "required") {
		t.Fatalf("MARKET edit must initially hide and disable price: %#v", marketMatch)
	}
}

func TestOrderFormStylesEnforceEqualCardsAndNarrowReflow(t *testing.T) {
	cssBytes, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBytes)
	for _, declaration := range []string{
		"grid-auto-rows: 1fr",
		".choice-cards label { position: relative; min-width: 0; display: grid",
		".choice-cards label > span { height: 100%",
		".order-detail-stack { display: grid; gap: 22px; }",
		".order-form > .form-section:last-of-type { border-bottom: 0; }",
		".price-field { width: 100%; max-width: none; }",
		".order-form > .form-actions { flex-direction: column-reverse; }",
		".order-form > .form-actions .button { width: 100%; }",
	} {
		if !strings.Contains(css, declaration) {
			t.Fatalf("order-form CSS is missing visual invariant %q", declaration)
		}
	}
}

func executeOrderFormTemplate(t *testing.T, app *App, name string, data pageData) string {
	t.Helper()
	var output bytes.Buffer
	if err := app.templates.ExecuteTemplate(&output, name, data); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func adminOrderFormData(orderType string) pageData {
	return pageData{
		Now: time.Now(), Admin: domain.Admin{ID: 1, Username: "admin"},
		EditTrader: domain.Trader{ID: 2, DisplayClientID: "AB1234"},
		EditOrder: domain.QueuedOrder{
			ExecutionDate: "2026-08-10", Position: 1, State: "QUEUED",
			Params: domain.OrderParams{
				Exchange: "NSE", TradingSymbol: "ACME", TransactionType: "SELL", Product: "CNC",
				OrderType: orderType, Quantity: 2, Price: "10.50", Validity: "DAY",
			},
		},
		MinExecutionDate: "2026-08-10", QueueRevision: 1, FormAction: "/admin/orders/1/edit",
	}
}
