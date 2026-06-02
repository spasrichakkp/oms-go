package platform

import (
	"strings"
	"testing"
	"time"

	"oms/internal/auth"
)

func TestLoadConfigUsesLockedAuthModeByDefault(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/oms")
	t.Setenv("OMS_AUTH_MODE", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.AuthMode != auth.ModeLocked {
		t.Fatalf("expected locked auth mode, got %q", cfg.AuthMode)
	}

	if cfg.ReadTimeout != defaultReadTimeout {
		t.Fatalf("expected default read timeout %s, got %s", defaultReadTimeout, cfg.ReadTimeout)
	}

	if cfg.WriteTimeout != defaultWriteTimeout {
		t.Fatalf("expected default write timeout %s, got %s", defaultWriteTimeout, cfg.WriteTimeout)
	}

	if cfg.IdleTimeout != defaultIdleTimeout {
		t.Fatalf("expected default idle timeout %s, got %s", defaultIdleTimeout, cfg.IdleTimeout)
	}
}

func TestLoadConfigAcceptsDevAuthMode(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/oms")
	t.Setenv("OMS_AUTH_MODE", "dev")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.AuthMode != auth.ModeDev {
		t.Fatalf("expected dev auth mode, got %q", cfg.AuthMode)
	}
}

func TestLoadConfigAcceptsCustomHTTPTimeouts(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/oms")
	t.Setenv("HTTP_READ_TIMEOUT", "7s")
	t.Setenv("HTTP_WRITE_TIMEOUT", "12s")
	t.Setenv("HTTP_IDLE_TIMEOUT", "90s")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.ReadTimeout != 7*time.Second {
		t.Fatalf("expected read timeout 7s, got %s", cfg.ReadTimeout)
	}

	if cfg.WriteTimeout != 12*time.Second {
		t.Fatalf("expected write timeout 12s, got %s", cfg.WriteTimeout)
	}

	if cfg.IdleTimeout != 90*time.Second {
		t.Fatalf("expected idle timeout 90s, got %s", cfg.IdleTimeout)
	}
}

func TestLoadConfigRejectsUnsupportedAuthMode(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/oms")
	t.Setenv("OMS_AUTH_MODE", "production")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected unsupported auth mode error")
	}
}

func TestLoadConfigRejectsInvalidHTTPTimeouts(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/oms")

	testCases := []struct {
		name      string
		envName   string
		envValue  string
		wantError string
	}{
		{
			name:      "invalid read timeout syntax",
			envName:   "HTTP_READ_TIMEOUT",
			envValue:  "later",
			wantError: "HTTP_READ_TIMEOUT must be a valid duration",
		},
		{
			name:      "zero write timeout",
			envName:   "HTTP_WRITE_TIMEOUT",
			envValue:  "0s",
			wantError: "HTTP_WRITE_TIMEOUT must be a positive duration",
		},
		{
			name:      "negative idle timeout",
			envName:   "HTTP_IDLE_TIMEOUT",
			envValue:  "-1s",
			wantError: "HTTP_IDLE_TIMEOUT must be a positive duration",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.envName, tc.envValue)

			_, err := LoadConfig()
			if err == nil {
				t.Fatal("expected timeout parsing error")
			}

			if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected error containing %q, got %q", tc.wantError, err.Error())
			}
		})
	}
}

func TestLoadConfigAcceptsJWTAuthMode(t *testing.T) {
	setValidJWTEnv(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.AuthMode != auth.ModeJWT || cfg.AuthConfig.Mode != auth.ModeJWT {
		t.Fatalf("expected JWT auth mode, got %#v", cfg.AuthConfig)
	}
	if cfg.AuthConfig.SubjectClaim != "sub" {
		t.Fatalf("expected default subject claim sub, got %q", cfg.AuthConfig.SubjectClaim)
	}
	if len(cfg.AuthConfig.AllowedAlgorithms) != 2 || cfg.AuthConfig.AllowedAlgorithms[0] != "RS256" || cfg.AuthConfig.AllowedAlgorithms[1] != "ES256" {
		t.Fatalf("expected allowed algorithms to be parsed, got %#v", cfg.AuthConfig.AllowedAlgorithms)
	}
	if cfg.AuthConfig.HTTPTimeout != defaultJWTHTTPTimeout {
		t.Fatalf("expected default JWT HTTP timeout %s, got %s", defaultJWTHTTPTimeout, cfg.AuthConfig.HTTPTimeout)
	}
}

func TestLoadConfigAcceptsCustomJWTHTTPTimeout(t *testing.T) {
	setValidJWTEnv(t)
	t.Setenv("OMS_JWT_HTTP_TIMEOUT", "3s")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.AuthConfig.HTTPTimeout != 3*time.Second {
		t.Fatalf("expected JWT HTTP timeout 3s, got %s", cfg.AuthConfig.HTTPTimeout)
	}
}

func TestLoadConfigRejectsInvalidJWTAuthConfig(t *testing.T) {
	testCases := []struct {
		name      string
		envName   string
		envValue  string
		wantError string
	}{
		{
			name:      "missing issuer",
			envName:   "OMS_JWT_ISSUER",
			envValue:  "",
			wantError: "OMS_JWT_ISSUER is required",
		},
		{
			name:      "insecure issuer",
			envName:   "OMS_JWT_ISSUER",
			envValue:  "http://issuer.example.test",
			wantError: "OMS_JWT_ISSUER must be a valid HTTPS URL",
		},
		{
			name:      "unsafe algorithm",
			envName:   "OMS_JWT_ALLOWED_ALGS",
			envValue:  "HS256",
			wantError: "OMS_JWT_ALLOWED_ALGS contains unsupported algorithm",
		},
		{
			name:      "none algorithm",
			envName:   "OMS_JWT_ALLOWED_ALGS",
			envValue:  "none",
			wantError: "OMS_JWT_ALLOWED_ALGS contains unsupported algorithm",
		},
		{
			name:      "duplicate role values",
			envName:   "OMS_JWT_ROLE_ADMIN_VALUE",
			envValue:  "customer",
			wantError: "OMS JWT role values must be distinct",
		},
		{
			name:      "invalid HTTP timeout syntax",
			envName:   "OMS_JWT_HTTP_TIMEOUT",
			envValue:  "later",
			wantError: "OMS_JWT_HTTP_TIMEOUT must be a valid duration",
		},
		{
			name:      "zero HTTP timeout",
			envName:   "OMS_JWT_HTTP_TIMEOUT",
			envValue:  "0s",
			wantError: "OMS_JWT_HTTP_TIMEOUT must be a positive duration",
		},
		{
			name:      "negative HTTP timeout",
			envName:   "OMS_JWT_HTTP_TIMEOUT",
			envValue:  "-1s",
			wantError: "OMS_JWT_HTTP_TIMEOUT must be a positive duration",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			setValidJWTEnv(t)
			t.Setenv(tc.envName, tc.envValue)

			_, err := LoadConfig()
			if err == nil {
				t.Fatal("expected JWT config error")
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected error containing %q, got %q", tc.wantError, err.Error())
			}
		})
	}
}

func TestLoadConfigRejectsMultipleJWTVerifierSources(t *testing.T) {
	setValidJWTEnv(t)
	t.Setenv("OMS_OIDC_DISCOVERY_URL", "https://issuer.example.test/.well-known/openid-configuration")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected verifier source error")
	}
	if !strings.Contains(err.Error(), "exactly one of OMS_JWT_JWKS_URL or OMS_OIDC_DISCOVERY_URL") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func setValidJWTEnv(t *testing.T) {
	t.Helper()

	t.Setenv("DATABASE_URL", "postgres://localhost/oms")
	t.Setenv("OMS_AUTH_MODE", "jwt")
	t.Setenv("OMS_JWT_ISSUER", "https://issuer.example.test")
	t.Setenv("OMS_JWT_AUDIENCE", "oms-api")
	t.Setenv("OMS_JWT_JWKS_URL", "https://issuer.example.test/jwks")
	t.Setenv("OMS_OIDC_DISCOVERY_URL", "")
	t.Setenv("OMS_JWT_ALLOWED_ALGS", "RS256,ES256")
	t.Setenv("OMS_JWT_SUBJECT_CLAIM", "")
	t.Setenv("OMS_JWT_CUSTOMER_ID_CLAIM", "customer_id")
	t.Setenv("OMS_JWT_ROLE_CLAIM", "role")
	t.Setenv("OMS_JWT_ROLE_CUSTOMER_VALUE", "customer")
	t.Setenv("OMS_JWT_ROLE_ADMIN_VALUE", "admin")
	t.Setenv("OMS_JWT_ROLE_SYSTEM_VALUE", "system")
	t.Setenv("OMS_JWT_HTTP_TIMEOUT", "")
}
