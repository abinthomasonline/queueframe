package config

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Address           string
	BaseURL           string
	DatabasePath      string
	MasterKey         []byte
	AdminUsername     string
	AdminPassword     string
	SecureCookies     bool
	DryRun            bool
	DispatchClock     string
	PrepareBefore     time.Duration
	WarmBefore        time.Duration
	LateCutoff        time.Duration
	SessionLifetime   time.Duration
	ExpectedPublicIP  string
	TrustedProxyCIDRs []*net.IPNet
	RedirectURL       string
}

// minimumWarmBefore leaves one second beyond the production broker client's
// seven-second HTTP timeout. Keep this aligned with the production client if
// that timeout changes.
const minimumWarmBefore = 8 * time.Second

func Load() (Config, error) {
	prepareBefore, err := envDurationCompat("QUEUEFRAME_PREPARE_BEFORE", "SELLATOPEN_PREPARE_BEFORE", 5*time.Minute)
	if err != nil {
		return Config{}, err
	}
	warmBefore, err := envDurationCompat("QUEUEFRAME_WARM_BEFORE", "SELLATOPEN_WARM_BEFORE", 10*time.Second)
	if err != nil {
		return Config{}, err
	}
	lateCutoff, err := envDurationCompat("QUEUEFRAME_LATE_CUTOFF", "SELLATOPEN_LATE_CUTOFF", 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	sessionLifetime, err := envDurationCompat("QUEUEFRAME_WEB_SESSION_TTL", "SELLATOPEN_WEB_SESSION_TTL", 12*time.Hour)
	if err != nil {
		return Config{}, err
	}
	secureCookies, err := envBoolCompat("QUEUEFRAME_SECURE_COOKIES", "SELLATOPEN_SECURE_COOKIES", false)
	if err != nil {
		return Config{}, err
	}
	dryRun, err := envBoolCompat("QUEUEFRAME_DRY_RUN", "SELLATOPEN_DRY_RUN", true)
	if err != nil {
		return Config{}, err
	}
	trustedProxyCIDRs, err := envCIDRs("QUEUEFRAME_TRUSTED_PROXY_CIDRS")
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Address:           envCompat("QUEUEFRAME_ADDR", "SELLATOPEN_ADDR", "127.0.0.1:8080"),
		BaseURL:           strings.TrimRight(envCompat("QUEUEFRAME_BASE_URL", "SELLATOPEN_BASE_URL", "http://localhost:8080"), "/"),
		DatabasePath:      envCompat("QUEUEFRAME_DB", "SELLATOPEN_DB", "./data/queueframe.db"),
		AdminUsername:     envCompat("QUEUEFRAME_ADMIN_USER", "SELLATOPEN_ADMIN_USER", "admin"),
		AdminPassword:     envCompat("QUEUEFRAME_ADMIN_PASSWORD", "SELLATOPEN_ADMIN_PASSWORD", ""),
		SecureCookies:     secureCookies,
		DryRun:            dryRun,
		DispatchClock:     envCompat("QUEUEFRAME_DISPATCH_TIME", "SELLATOPEN_DISPATCH_TIME", "09:00:00"),
		PrepareBefore:     prepareBefore,
		WarmBefore:        warmBefore,
		LateCutoff:        lateCutoff,
		SessionLifetime:   sessionLifetime,
		ExpectedPublicIP:  envCompat("QUEUEFRAME_EXPECTED_PUBLIC_IP", "SELLATOPEN_EXPECTED_PUBLIC_IP", ""),
		TrustedProxyCIDRs: trustedProxyCIDRs,
	}

	if _, err := time.Parse("15:04:05", cfg.DispatchClock); err != nil {
		return Config{}, fmt.Errorf("%s: %w", envName("QUEUEFRAME_DISPATCH_TIME", "SELLATOPEN_DISPATCH_TIME"), err)
	}
	baseURL, err := url.Parse(cfg.BaseURL)
	if err != nil || baseURL.Host == "" || (baseURL.Scheme != "http" && baseURL.Scheme != "https") ||
		baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" || (baseURL.Path != "" && baseURL.Path != "/") {
		return Config{}, fmt.Errorf("%s must be an absolute http(s) URL", envName("QUEUEFRAME_BASE_URL", "SELLATOPEN_BASE_URL"))
	}
	if baseURL.Scheme == "https" && !cfg.SecureCookies {
		return Config{}, fmt.Errorf("%s must be true when %s uses HTTPS", envName("QUEUEFRAME_SECURE_COOKIES", "SELLATOPEN_SECURE_COOKIES"), envName("QUEUEFRAME_BASE_URL", "SELLATOPEN_BASE_URL"))
	}
	if baseURL.Scheme == "http" {
		host := strings.ToLower(baseURL.Hostname())
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return Config{}, fmt.Errorf("plain HTTP is allowed only for a loopback/localhost %s; use HTTPS for remote access", envName("QUEUEFRAME_BASE_URL", "SELLATOPEN_BASE_URL"))
		}
	}
	callback := *baseURL
	callback.Path = strings.TrimRight(callback.Path, "/") + "/auth/kite/callback"
	callback.RawPath = ""
	callback.RawQuery = ""
	callback.Fragment = ""
	cfg.RedirectURL = callback.String()
	if cfg.ExpectedPublicIP != "" && net.ParseIP(cfg.ExpectedPublicIP) == nil {
		return Config{}, fmt.Errorf("%s must be one IPv4 or IPv6 address", envName("QUEUEFRAME_EXPECTED_PUBLIC_IP", "SELLATOPEN_EXPECTED_PUBLIC_IP"))
	}
	if cfg.PrepareBefore <= 0 {
		return Config{}, fmt.Errorf("%s must be positive", envName("QUEUEFRAME_PREPARE_BEFORE", "SELLATOPEN_PREPARE_BEFORE"))
	}
	if cfg.WarmBefore <= 0 {
		return Config{}, fmt.Errorf("%s must be positive", envName("QUEUEFRAME_WARM_BEFORE", "SELLATOPEN_WARM_BEFORE"))
	}
	if cfg.WarmBefore < minimumWarmBefore {
		return Config{}, fmt.Errorf("%s must be at least %s", envName("QUEUEFRAME_WARM_BEFORE", "SELLATOPEN_WARM_BEFORE"), minimumWarmBefore)
	}
	if cfg.WarmBefore >= cfg.PrepareBefore {
		return Config{}, fmt.Errorf("%s must be smaller than %s", envName("QUEUEFRAME_WARM_BEFORE", "SELLATOPEN_WARM_BEFORE"), envName("QUEUEFRAME_PREPARE_BEFORE", "SELLATOPEN_PREPARE_BEFORE"))
	}
	if cfg.LateCutoff <= 0 {
		return Config{}, fmt.Errorf("%s must be positive", envName("QUEUEFRAME_LATE_CUTOFF", "SELLATOPEN_LATE_CUTOFF"))
	}
	if cfg.SessionLifetime <= 0 {
		return Config{}, fmt.Errorf("%s must be positive", envName("QUEUEFRAME_WEB_SESSION_TTL", "SELLATOPEN_WEB_SESSION_TTL"))
	}
	keyText := envCompat("QUEUEFRAME_MASTER_KEY", "SELLATOPEN_MASTER_KEY", "")
	if keyText == "" {
		return Config{}, fmt.Errorf("%s is required (base64-encoded 32-byte key)", envName("QUEUEFRAME_MASTER_KEY", "SELLATOPEN_MASTER_KEY"))
	}
	key, err := base64.StdEncoding.DecodeString(keyText)
	if err != nil || len(key) != 32 {
		return Config{}, fmt.Errorf("%s must be a base64-encoded 32-byte key", envName("QUEUEFRAME_MASTER_KEY", "SELLATOPEN_MASTER_KEY"))
	}
	cfg.MasterKey = key
	return cfg, nil
}

// The QUEUEFRAME names are authoritative. Legacy SELLATOPEN names remain as a
// migration bridge for existing private deployments and may be removed in a
// future major release.
func envCompat(primary, legacy, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(primary)); value != "" {
		return value
	}
	return env(legacy, fallback)
}

func selectedEnv(primary, legacy string) (string, string) {
	if value := strings.TrimSpace(os.Getenv(primary)); value != "" {
		return primary, value
	}
	return legacy, strings.TrimSpace(os.Getenv(legacy))
}

func envName(primary, legacy string) string {
	name, _ := selectedEnv(primary, legacy)
	if strings.TrimSpace(os.Getenv(name)) == "" {
		return primary
	}
	return name
}

func envBoolCompat(primary, legacy string, fallback bool) (bool, error) {
	name, value := selectedEnv(primary, legacy)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a valid boolean: %w", name, err)
	}
	return parsed, nil
}

func envDurationCompat(primary, legacy string, fallback time.Duration) (time.Duration, error) {
	name, value := selectedEnv(primary, legacy)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid Go duration: %w", name, err)
	}
	return parsed, nil
}

func envCIDRs(name string) ([]*net.IPNet, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	networks := make([]*net.IPNet, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("%s must contain only comma-separated CIDR ranges", name)
		}
		_, network, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("%s contains invalid CIDR %q: %w", name, part, err)
		}
		networks = append(networks, network)
	}
	return networks, nil
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a valid boolean: %w", name, err)
	}
	return parsed, nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid Go duration: %w", name, err)
	}
	return parsed, nil
}
