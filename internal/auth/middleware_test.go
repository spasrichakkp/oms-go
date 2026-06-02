package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIdentityMiddlewareDevModeStoresCustomerIdentity(t *testing.T) {
	t.Parallel()

	var gotRole Role
	var gotCustomer bool

	handler := IdentityMiddleware(ModeDev)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role, ok := RoleFromContext(r.Context())
		if !ok {
			t.Fatalf("expected role in context")
		}
		gotRole = role

		_, gotCustomer = CustomerIDFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req.Header.Set("X-OMS-Role", "customer")
	req.Header.Set("X-OMS-Customer-ID", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
	if gotRole != RoleCustomer || !gotCustomer {
		t.Fatalf("expected customer identity in context")
	}
}

func TestIdentityMiddlewareLockedModeRejectsForgedDevHeaders(t *testing.T) {
	t.Parallel()

	handler := IdentityMiddleware(ModeLocked)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected next handler call")
	}))

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req.Header.Set("X-OMS-Role", "ADMIN")
	req.Header.Set("X-OMS-Customer-ID", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assertAuthErrorResponse(t, rec, http.StatusUnauthorized, "unauthorized")
}

func TestIdentityMiddlewareDevModeRejectsMissingIdentity(t *testing.T) {
	t.Parallel()

	handler := IdentityMiddleware(ModeDev)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected next handler call")
	}))

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assertAuthErrorResponse(t, rec, http.StatusUnauthorized, "unauthorized")
}

func TestIdentityMiddlewareDevModeRejectsCustomerWithoutCustomerID(t *testing.T) {
	t.Parallel()

	handler := IdentityMiddleware(ModeDev)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected next handler call")
	}))

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req.Header.Set("X-OMS-Role", "CUSTOMER")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assertAuthErrorResponse(t, rec, http.StatusUnauthorized, "unauthorized")
}

func TestRequireRoleRejectsInsufficientRole(t *testing.T) {
	t.Parallel()

	handler := IdentityMiddleware(ModeDev)(RequireRole(RoleAdmin, RoleSystem)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected next handler call")
	})))

	req := httptest.NewRequest(http.MethodPatch, "/orders/123/status", nil)
	req.Header.Set("X-OMS-Role", "CUSTOMER")
	req.Header.Set("X-OMS-Customer-ID", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assertAuthErrorResponse(t, rec, http.StatusForbidden, "forbidden")
}

func TestRequireRoleAllowsAdmin(t *testing.T) {
	t.Parallel()

	handler := IdentityMiddleware(ModeDev)(RequireRole(RoleAdmin, RoleSystem)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))

	req := httptest.NewRequest(http.MethodPatch, "/orders/123/status", nil)
	req.Header.Set("X-OMS-Role", "ADMIN")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
}

func assertAuthErrorResponse(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()

	if rec.Code != status {
		t.Fatalf("expected %d, got %d", status, rec.Code)
	}

	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode auth error response: %v", err)
	}

	if body.Error.Code != code {
		t.Fatalf("expected code %q, got %q", code, body.Error.Code)
	}
}
