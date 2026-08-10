package config

import (
	"net"
	"strings"
	"testing"
)

func TestLoadValidatesSchedulingDurations(t *testing.T) {
	tests := []struct {
		name          string
		prepareBefore string
		warmBefore    string
		lateCutoff    string
		wantErr       string
	}{
		{name: "valid", prepareBefore: "5m", warmBefore: "10s", lateCutoff: "5s"},
		{name: "minimum warm lead", prepareBefore: "5m", warmBefore: "8s", lateCutoff: "5s"},
		{name: "prepare zero", prepareBefore: "0s", warmBefore: "10s", lateCutoff: "5s", wantErr: "SELLATOPEN_PREPARE_BEFORE must be positive"},
		{name: "prepare negative", prepareBefore: "-1s", warmBefore: "10s", lateCutoff: "5s", wantErr: "SELLATOPEN_PREPARE_BEFORE must be positive"},
		{name: "warm zero", prepareBefore: "5m", warmBefore: "0s", lateCutoff: "5s", wantErr: "SELLATOPEN_WARM_BEFORE must be positive"},
		{name: "warm negative", prepareBefore: "5m", warmBefore: "-1s", lateCutoff: "5s", wantErr: "SELLATOPEN_WARM_BEFORE must be positive"},
		{name: "warm shorter than broker timeout margin", prepareBefore: "5m", warmBefore: "7999ms", lateCutoff: "5s", wantErr: "SELLATOPEN_WARM_BEFORE must be at least 8s"},
		{name: "warm equals preparation", prepareBefore: "5m", warmBefore: "5m", lateCutoff: "5s", wantErr: "SELLATOPEN_WARM_BEFORE must be smaller"},
		{name: "warm precedes preparation", prepareBefore: "5m", warmBefore: "6m", lateCutoff: "5s", wantErr: "SELLATOPEN_WARM_BEFORE must be smaller"},
		{name: "late cutoff zero", prepareBefore: "5m", warmBefore: "10s", lateCutoff: "0s", wantErr: "SELLATOPEN_LATE_CUTOFF must be positive"},
		{name: "late cutoff negative", prepareBefore: "5m", warmBefore: "10s", lateCutoff: "-1s", wantErr: "SELLATOPEN_LATE_CUTOFF must be positive"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setValidEnvironment(t)
			t.Setenv("SELLATOPEN_PREPARE_BEFORE", test.prepareBefore)
			t.Setenv("SELLATOPEN_WARM_BEFORE", test.warmBefore)
			t.Setenv("SELLATOPEN_LATE_CUTOFF", test.lateCutoff)

			_, err := Load()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Load error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadRejectsMalformedDurations(t *testing.T) {
	tests := []string{
		"SELLATOPEN_PREPARE_BEFORE",
		"SELLATOPEN_WARM_BEFORE",
		"SELLATOPEN_LATE_CUTOFF",
		"SELLATOPEN_WEB_SESSION_TTL",
	}
	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			setValidEnvironment(t)
			t.Setenv(name, "10seconds")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), name+" must be a valid Go duration") {
				t.Fatalf("Load error = %v, want named duration error", err)
			}
		})
	}
}

func TestLoadRejectsMalformedBooleans(t *testing.T) {
	for _, name := range []string{"SELLATOPEN_SECURE_COOKIES", "SELLATOPEN_DRY_RUN"} {
		t.Run(name, func(t *testing.T) {
			setValidEnvironment(t)
			t.Setenv(name, "definitely")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), name+" must be a valid boolean") {
				t.Fatalf("Load error = %v, want named boolean error", err)
			}
		})
	}
}

func TestLoadRejectsNonPositiveSessionTTL(t *testing.T) {
	for _, value := range []string{"0s", "-1s"} {
		t.Run(value, func(t *testing.T) {
			setValidEnvironment(t)
			t.Setenv("SELLATOPEN_WEB_SESSION_TTL", value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "SELLATOPEN_WEB_SESSION_TTL must be positive") {
				t.Fatalf("Load error = %v, want positive session TTL error", err)
			}
		})
	}
}

func TestQueueframeNamesTakePrecedenceAndDeriveTrustedCallback(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("QUEUEFRAME_BASE_URL", "https://orders.example")
	t.Setenv("QUEUEFRAME_SECURE_COOKIES", "true")
	t.Setenv("QUEUEFRAME_EXPECTED_PUBLIC_IP", "203.0.113.8")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "https://orders.example" || cfg.RedirectURL != "https://orders.example/auth/kite/callback" || cfg.ExpectedPublicIP != "203.0.113.8" {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestLoadRejectsInvalidPublicIPAndBasePath(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("QUEUEFRAME_EXPECTED_PUBLIC_IP", "orders.example")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "QUEUEFRAME_EXPECTED_PUBLIC_IP") {
		t.Fatalf("invalid IP error = %v", err)
	}
	setValidEnvironment(t)
	t.Setenv("QUEUEFRAME_BASE_URL", "http://localhost:8080/subpath")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "QUEUEFRAME_BASE_URL") {
		t.Fatalf("base path error = %v", err)
	}
}

func TestLoadParsesTrustedProxyCIDRs(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("QUEUEFRAME_TRUSTED_PROXY_CIDRS", "172.30.255.0/24, 127.0.0.0/8,::1/128")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TrustedProxyCIDRs) != 3 || !cfg.TrustedProxyCIDRs[0].Contains(net.ParseIP("172.30.255.7")) ||
		!cfg.TrustedProxyCIDRs[1].Contains(net.ParseIP("127.0.0.1")) || !cfg.TrustedProxyCIDRs[2].Contains(net.ParseIP("::1")) {
		t.Fatalf("trusted proxy CIDRs = %#v", cfg.TrustedProxyCIDRs)
	}

	setValidEnvironment(t)
	t.Setenv("QUEUEFRAME_TRUSTED_PROXY_CIDRS", "172.30.255.7")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "QUEUEFRAME_TRUSTED_PROXY_CIDRS") {
		t.Fatalf("invalid trusted proxy error = %v", err)
	}
}

func setValidEnvironment(t *testing.T) {
	t.Helper()
	values := map[string]string{
		"SELLATOPEN_ADDR":               "127.0.0.1:8080",
		"SELLATOPEN_BASE_URL":           "http://localhost:8080",
		"SELLATOPEN_DB":                 "./data/test.db",
		"SELLATOPEN_ADMIN_USER":         "admin",
		"SELLATOPEN_ADMIN_PASSWORD":     "a-long-admin-password",
		"SELLATOPEN_SECURE_COOKIES":     "false",
		"SELLATOPEN_DRY_RUN":            "true",
		"SELLATOPEN_DISPATCH_TIME":      "09:00:00",
		"SELLATOPEN_PREPARE_BEFORE":     "5m",
		"SELLATOPEN_WARM_BEFORE":        "10s",
		"SELLATOPEN_LATE_CUTOFF":        "5s",
		"SELLATOPEN_WEB_SESSION_TTL":    "12h",
		"SELLATOPEN_EXPECTED_PUBLIC_IP": "",
		"SELLATOPEN_MASTER_KEY":         "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	}
	for name, value := range values {
		t.Setenv(name, value)
	}
	for _, name := range []string{
		"QUEUEFRAME_ADDR", "QUEUEFRAME_BASE_URL", "QUEUEFRAME_DB", "QUEUEFRAME_ADMIN_USER",
		"QUEUEFRAME_ADMIN_PASSWORD", "QUEUEFRAME_SECURE_COOKIES", "QUEUEFRAME_DRY_RUN",
		"QUEUEFRAME_DISPATCH_TIME", "QUEUEFRAME_PREPARE_BEFORE", "QUEUEFRAME_WARM_BEFORE",
		"QUEUEFRAME_LATE_CUTOFF", "QUEUEFRAME_WEB_SESSION_TTL", "QUEUEFRAME_EXPECTED_PUBLIC_IP",
		"QUEUEFRAME_MASTER_KEY", "QUEUEFRAME_TRUSTED_PROXY_CIDRS",
	} {
		t.Setenv(name, "")
	}
}
