package web

import (
	"os"
	"strings"
	"testing"
)

func TestExternalDocumentationLinksOpenSafely(t *testing.T) {
	t.Parallel()
	tests := []struct {
		file string
		href string
	}{
		{"templates/login.html", `https://developers.kite.trade/`},
		{"templates/login.html", `https://support.zerodha.com/category/trading-and-markets/general-kite/kite-api/articles/static-ip`},
		{"templates/accounts.html", `{{.Trader.PolicyAttestation.SourceURL}}`},
		{"templates/accounts.html", `https://support.zerodha.com/category/your-zerodha-account/your-profile/ddpi/articles/activate-ddpi`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.href, func(t *testing.T) {
			t.Parallel()
			content, err := os.ReadFile(test.file)
			if err != nil {
				t.Fatal(err)
			}
			want := `href="` + test.href + `" target="_blank" rel="noopener noreferrer"`
			if !strings.Contains(string(content), want) {
				t.Fatalf("%s does not contain safe external anchor attributes for %s", test.file, test.href)
			}
		})
	}
}

func TestKiteAuthenticationFormsStayInCurrentTab(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("templates/login.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{`action="/signup"`, `action="/signin"`} {
		start := strings.Index(string(content), action)
		if start < 0 {
			t.Fatalf("missing Kite authentication form %s", action)
		}
		formStart := strings.LastIndex(string(content[:start]), "<form")
		formEnd := strings.Index(string(content[start:]), ">")
		if formStart < 0 || formEnd < 0 {
			t.Fatalf("could not locate form tag for %s", action)
		}
		tag := string(content[formStart : start+formEnd+1])
		if strings.Contains(tag, "target=") {
			t.Fatalf("Kite authentication form must remain in the current tab: %s", tag)
		}
	}
}
