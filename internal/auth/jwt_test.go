package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestJWTIdentityMiddlewareAcceptsValidCustomerToken(t *testing.T) {
	t.Parallel()

	fixture := newJWTFixture(t)
	customerID := "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	token := fixture.sign(t, fixture.key, "RS256", map[string]any{
		"sub":         "customer-subject",
		"role":        "customer",
		"customer_id": customerID,
	})

	var got Identity
	rec := fixture.serve(t, fixture.config(), token, func(identity Identity) {
		got = identity
	})

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if got.Role != RoleCustomer || !got.HasCustomerID || got.CustomerID.String() != customerID {
		t.Fatalf("expected customer identity, got %#v", got)
	}
	if !got.HasSubject || got.Subject != "customer-subject" {
		t.Fatalf("expected verified subject, got %#v", got)
	}
}

func TestJWTIdentityMiddlewareAcceptsPrivilegedTokens(t *testing.T) {
	t.Parallel()

	fixture := newJWTFixture(t)
	for _, tc := range []struct {
		name      string
		role      string
		subjectID string
		want      Role
	}{
		{name: "admin", role: "admin", subjectID: "6ba7b810-9dad-11d1-80b4-00c04fd430c8", want: RoleAdmin},
		{name: "system", role: "system", subjectID: "6ba7b811-9dad-11d1-80b4-00c04fd430c8", want: RoleSystem},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := fixture.sign(t, fixture.key, "RS256", map[string]any{
				"sub":  tc.subjectID,
				"role": tc.role,
			})

			var got Identity
			rec := fixture.serve(t, fixture.config(), token, func(identity Identity) {
				got = identity
			})

			if rec.Code != http.StatusNoContent {
				t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
			}
			if got.Role != tc.want || !got.HasSubject || got.Subject != tc.subjectID {
				t.Fatalf("expected privileged identity, got %#v", got)
			}
			if !got.HasSubjectID || got.SubjectID.String() != tc.subjectID {
				t.Fatalf("expected privileged subject ID, got %#v", got)
			}
		})
	}
}

func TestJWTIdentityMiddlewareRejectsInvalidTokensWithGenericError(t *testing.T) {
	t.Parallel()

	fixture := newJWTFixture(t)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate alternate RSA key: %v", err)
	}

	validClaims := map[string]any{
		"sub":         "customer-subject",
		"role":        "customer",
		"customer_id": "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
	}

	testCases := []struct {
		name   string
		token  string
		header string
	}{
		{name: "missing authorization"},
		{name: "malformed bearer", header: "Basic credentials"},
		{name: "forged dev headers without bearer"},
		{name: "invalid signature", token: fixture.sign(t, otherKey, "RS256", validClaims)},
		{name: "expired", token: fixture.sign(t, fixture.key, "RS256", mergeClaims(validClaims, map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}))},
		{name: "wrong issuer", token: fixture.sign(t, fixture.key, "RS256", mergeClaims(validClaims, map[string]any{"iss": "https://wrong.example.test"}))},
		{name: "wrong audience", token: fixture.sign(t, fixture.key, "RS256", mergeClaims(validClaims, map[string]any{"aud": "wrong-audience"}))},
		{name: "unsupported algorithm", token: fixture.sign(t, fixture.key, "ES256", validClaims)},
		{name: "missing subject", token: fixture.sign(t, fixture.key, "RS256", mergeClaims(validClaims, map[string]any{"sub": ""}))},
		{name: "missing role", token: fixture.sign(t, fixture.key, "RS256", mergeClaims(validClaims, map[string]any{"role": nil}))},
		{name: "ambiguous role", token: fixture.sign(t, fixture.key, "RS256", mergeClaims(validClaims, map[string]any{"role": []string{"customer", "admin"}}))},
		{name: "invalid customer id", token: fixture.sign(t, fixture.key, "RS256", mergeClaims(validClaims, map[string]any{"customer_id": "not-a-uuid"}))},
		{name: "invalid privileged subject id", token: fixture.sign(t, fixture.key, "RS256", map[string]any{"sub": "not-a-uuid", "role": "admin"})},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fixture.config()
			middleware, err := newIdentityMiddleware(context.Background(), fixture.server.Client(), cfg)
			if err != nil {
				t.Fatalf("initialize JWT middleware: %v", err)
			}

			handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("unexpected next handler call")
			}))
			req := httptest.NewRequest(http.MethodGet, "/orders", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			} else if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if tc.name == "forged dev headers without bearer" {
				req.Header.Set("X-OMS-Role", "ADMIN")
				req.Header.Set("X-OMS-Customer-ID", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			assertAuthErrorResponse(t, rec, http.StatusUnauthorized, "unauthorized")
			if (tc.token != "" && strings.Contains(rec.Body.String(), tc.token)) || strings.Contains(rec.Body.String(), "Authorization") {
				t.Fatalf("expected generic auth error body, got %s", rec.Body.String())
			}
		})
	}
}

func TestJWTIdentityMiddlewareAcceptsDiscoveryMetadata(t *testing.T) {
	t.Parallel()

	fixture := newJWTFixture(t)
	cfg := fixture.config()
	cfg.JWKSURL = ""
	cfg.OIDCDiscoveryURL = fixture.server.URL + "/.well-known/openid-configuration"

	token := fixture.sign(t, fixture.key, "RS256", map[string]any{
		"sub":         "customer-subject",
		"role":        "customer",
		"customer_id": "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
	})
	rec := fixture.serve(t, cfg, token, func(Identity) {})

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestJWTIdentityMiddlewareRejectsSlowDiscoveryMetadata(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(discoveryDocument{})
	}))
	t.Cleanup(server.Close)

	cfg := newJWTFixture(t).config()
	cfg.JWKSURL = ""
	cfg.OIDCDiscoveryURL = server.URL
	cfg.HTTPTimeout = 20 * time.Millisecond

	started := time.Now()
	_, err := newIdentityMiddleware(context.Background(), server.Client(), cfg)
	if err == nil {
		t.Fatal("expected slow discovery metadata error")
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("expected bounded discovery failure, took %s", time.Since(started))
	}
}

func TestJWTIdentityMiddlewareRejectsSlowJWKSMetadata(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	t.Cleanup(server.Close)

	cfg := newJWTFixture(t).config()
	cfg.JWKSURL = server.URL
	cfg.HTTPTimeout = 20 * time.Millisecond

	started := time.Now()
	_, err := newIdentityMiddleware(context.Background(), server.Client(), cfg)
	if err == nil {
		t.Fatal("expected slow JWKS metadata error")
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("expected bounded JWKS failure, took %s", time.Since(started))
	}
}

func TestJWTIdentityMiddlewareRejectsOversizedDiscoveryMetadata(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"issuer":"` + strings.Repeat("x", int(maxJWTMetadataResponseBytes)) + `"}`))
	}))
	t.Cleanup(server.Close)

	cfg := newJWTFixture(t).config()
	cfg.JWKSURL = ""
	cfg.OIDCDiscoveryURL = server.URL

	_, err := newIdentityMiddleware(context.Background(), server.Client(), cfg)
	if err == nil || !strings.Contains(err.Error(), errJWTMetadataResponseTooLarge.Error()) {
		t.Fatalf("expected oversized discovery metadata error, got %v", err)
	}
}

func TestJWTIdentityMiddlewareRejectsOversizedJWKSMetadata(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"keys":[` + strings.Repeat(" ", int(maxJWTMetadataResponseBytes))))
	}))
	t.Cleanup(server.Close)

	cfg := newJWTFixture(t).config()
	cfg.JWKSURL = server.URL

	_, err := newIdentityMiddleware(context.Background(), server.Client(), cfg)
	if err == nil || !strings.Contains(err.Error(), errJWTMetadataResponseTooLarge.Error()) {
		t.Fatalf("expected oversized JWKS metadata error, got %v", err)
	}
}

func TestJWTIdentityMiddlewareBoundsUnknownKeyRefresh(t *testing.T) {
	t.Parallel()

	fixture := newJWTFixture(t)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) > 1 {
			time.Sleep(100 * time.Millisecond)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{rsaJWK(&fixture.key.PublicKey)},
		})
	}))
	t.Cleanup(server.Close)

	cfg := fixture.config()
	cfg.JWKSURL = server.URL
	cfg.HTTPTimeout = 20 * time.Millisecond
	middleware, err := newIdentityMiddleware(context.Background(), server.Client(), cfg)
	if err != nil {
		t.Fatalf("initialize JWT middleware: %v", err)
	}
	token := fixture.signWithKID(t, fixture.key, "RS256", "unknown-key", map[string]any{
		"sub":         "customer-subject",
		"role":        "customer",
		"customer_id": "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
	})
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected next handler call")
	}))
	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	started := time.Now()
	handler.ServeHTTP(rec, req)
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("expected bounded unknown-key refresh failure, took %s", time.Since(started))
	}
	assertAuthErrorResponse(t, rec, http.StatusUnauthorized, "unauthorized")
	if strings.Contains(rec.Body.String(), token) || strings.Contains(rec.Body.String(), "Authorization") {
		t.Fatalf("expected generic auth error body, got %s", rec.Body.String())
	}
}

func TestJWTIdentityMiddlewareCustomerCannotAccessPrivilegedRoute(t *testing.T) {
	t.Parallel()

	fixture := newJWTFixture(t)
	token := fixture.sign(t, fixture.key, "RS256", map[string]any{
		"sub":         "customer-subject",
		"role":        "customer",
		"customer_id": "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
	})

	middleware, err := newIdentityMiddleware(context.Background(), fixture.server.Client(), fixture.config())
	if err != nil {
		t.Fatalf("initialize JWT middleware: %v", err)
	}
	handler := middleware(RequireRole(RoleAdmin, RoleSystem)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected next handler call")
	})))
	req := httptest.NewRequest(http.MethodPatch, "/orders/123/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assertAuthErrorResponse(t, rec, http.StatusForbidden, "forbidden")
}

func TestJWTIdentityMiddlewarePrivilegedRolesCanAccessPrivilegedRoute(t *testing.T) {
	t.Parallel()

	fixture := newJWTFixture(t)
	for index, role := range []string{"admin", "system"} {
		t.Run(role, func(t *testing.T) {
			token := fixture.sign(t, fixture.key, "RS256", map[string]any{
				"sub":  []string{"6ba7b810-9dad-11d1-80b4-00c04fd430c8", "6ba7b811-9dad-11d1-80b4-00c04fd430c8"}[index],
				"role": role,
			})

			middleware, err := newIdentityMiddleware(context.Background(), fixture.server.Client(), fixture.config())
			if err != nil {
				t.Fatalf("initialize JWT middleware: %v", err)
			}
			handler := middleware(RequireRole(RoleAdmin, RoleSystem)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})))
			req := httptest.NewRequest(http.MethodPatch, "/orders/123/status", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusNoContent {
				t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

type jwtFixture struct {
	t      *testing.T
	key    *rsa.PrivateKey
	server *httptest.Server
}

func newJWTFixture(t *testing.T) *jwtFixture {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	fixture := &jwtFixture{t: t, key: key}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jwks":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"keys": []map[string]string{rsaJWK(&key.PublicKey)},
			})
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(discoveryDocument{
				Issuer:  fixture.server.URL,
				JWKSURL: fixture.server.URL + "/jwks",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fixture.server.Close)

	return fixture
}

func (f *jwtFixture) config() Config {
	return Config{
		Mode:              ModeJWT,
		Issuer:            f.server.URL,
		Audience:          "oms-api",
		JWKSURL:           f.server.URL + "/jwks",
		AllowedAlgorithms: []string{"RS256"},
		SubjectClaim:      "sub",
		CustomerIDClaim:   "customer_id",
		RoleClaim:         "role",
		RoleCustomerValue: "customer",
		RoleAdminValue:    "admin",
		RoleSystemValue:   "system",
		HTTPTimeout:       5 * time.Second,
	}
}

func (f *jwtFixture) serve(t *testing.T, cfg Config, token string, capture func(Identity)) *httptest.ResponseRecorder {
	t.Helper()

	middleware, err := newIdentityMiddleware(context.Background(), f.server.Client(), cfg)
	if err != nil {
		t.Fatalf("initialize JWT middleware: %v", err)
	}
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := IdentityFromContext(r.Context())
		if !ok {
			t.Fatal("expected identity in context")
		}
		capture(identity)
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	return rec
}

func (f *jwtFixture) sign(t *testing.T, key *rsa.PrivateKey, algorithm string, overrides map[string]any) string {
	return f.signWithKID(t, key, algorithm, "test-key", overrides)
}

func (f *jwtFixture) signWithKID(t *testing.T, key *rsa.PrivateKey, algorithm, keyID string, overrides map[string]any) string {
	t.Helper()

	header := encodeJWTPart(t, map[string]any{
		"alg": algorithm,
		"kid": keyID,
		"typ": "JWT",
	})
	claims := map[string]any{
		"iss": f.server.URL,
		"aud": "oms-api",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	for name, value := range overrides {
		if value == nil {
			delete(claims, name)
			continue
		}
		claims[name] = value
	}
	payload := encodeJWTPart(t, claims)
	signingInput := header + "." + payload
	digest := crypto.SHA256.New()
	_, _ = digest.Write([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest.Sum(nil))
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func encodeJWTPart(t *testing.T, value any) string {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JWT part: %v", err)
	}

	return base64.RawURLEncoding.EncodeToString(data)
}

func rsaJWK(key *rsa.PublicKey) map[string]string {
	return map[string]string{
		"kty": "RSA",
		"kid": "test-key",
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}
}

func mergeClaims(base, overrides map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(overrides))
	for name, value := range base {
		merged[name] = value
	}
	for name, value := range overrides {
		merged[name] = value
	}
	return merged
}
