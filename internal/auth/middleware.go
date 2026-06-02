package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

const (
	devHeaderRole       = "X-OMS-Role"
	devHeaderCustomerID = "X-OMS-Customer-ID"
)

type Role string

const (
	RoleCustomer Role = "CUSTOMER"
	RoleAdmin    Role = "ADMIN"
	RoleSystem   Role = "SYSTEM"
)

type Mode string

const (
	ModeLocked Mode = ""
	ModeDev    Mode = "dev"
	ModeJWT    Mode = "jwt"
)

var (
	ErrMissingIdentity   = errors.New("missing identity")
	ErrInvalidRole       = errors.New("invalid role")
	ErrMissingCustomerID = errors.New("missing customer id")
	ErrInvalidCustomerID = errors.New("invalid customer id")
	ErrInsufficientRole  = errors.New("insufficient role")
)

type Identity struct {
	Role          Role
	CustomerID    uuid.UUID
	HasCustomerID bool
	Subject       string
	HasSubject    bool
	SubjectID     uuid.UUID
	HasSubjectID  bool
}

type contextKey string

const identityContextKey contextKey = "oms.auth.identity"

// IdentityMiddleware fails closed unless the explicitly local/dev-only mode is enabled.
// Dev trusted headers must be replaced by real JWT/OIDC middleware in production.
func IdentityMiddleware(mode Mode) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if mode != ModeDev {
				writeJSONError(w, http.StatusUnauthorized, "unauthorized", http.StatusText(http.StatusUnauthorized))
				return
			}

			identity, err := identityFromHeaders(r.Header)
			if err != nil {
				writeJSONError(w, http.StatusUnauthorized, "unauthorized", http.StatusText(http.StatusUnauthorized))
				return
			}

			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), identity)))
		})
	}
}

func SubjectFromContext(ctx context.Context) (string, bool) {
	identity, ok := IdentityFromContext(ctx)
	if !ok || !identity.HasSubject {
		return "", false
	}

	return identity.Subject, true
}

func RequireRole(allowed ...Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity, ok := IdentityFromContext(r.Context())
			if !ok {
				writeJSONError(w, http.StatusUnauthorized, "unauthorized", http.StatusText(http.StatusUnauthorized))
				return
			}

			for _, role := range allowed {
				if identity.Role == role {
					next.ServeHTTP(w, r)
					return
				}
			}

			writeJSONError(w, http.StatusForbidden, "forbidden", http.StatusText(http.StatusForbidden))
		})
	}
}

func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityContextKey, identity)
}

func IdentityFromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityContextKey).(Identity)
	return identity, ok
}

func CustomerIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	identity, ok := IdentityFromContext(ctx)
	if !ok || !identity.HasCustomerID {
		return uuid.UUID{}, false
	}

	return identity.CustomerID, true
}

func RoleFromContext(ctx context.Context) (Role, bool) {
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		return "", false
	}

	return identity.Role, true
}

func identityFromHeaders(header http.Header) (Identity, error) {
	roleValue := strings.TrimSpace(header.Get(devHeaderRole))
	if roleValue == "" {
		return Identity{}, ErrMissingIdentity
	}

	role := Role(strings.ToUpper(roleValue))
	switch role {
	case RoleCustomer:
		customerIDValue := strings.TrimSpace(header.Get(devHeaderCustomerID))
		if customerIDValue == "" {
			return Identity{}, ErrMissingCustomerID
		}

		customerID, err := uuid.Parse(customerIDValue)
		if err != nil {
			return Identity{}, ErrInvalidCustomerID
		}

		return Identity{
			Role:          role,
			CustomerID:    customerID,
			HasCustomerID: true,
		}, nil
	case RoleAdmin, RoleSystem:
		customerIDValue := strings.TrimSpace(header.Get(devHeaderCustomerID))
		if customerIDValue == "" {
			return Identity{
				Role: role,
			}, nil
		}

		customerID, err := uuid.Parse(customerIDValue)
		if err != nil {
			return Identity{}, ErrInvalidCustomerID
		}

		return Identity{
			Role:          role,
			CustomerID:    customerID,
			HasCustomerID: true,
		}, nil
	default:
		return Identity{}, ErrInvalidRole
	}
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}
