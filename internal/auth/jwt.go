package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
)

type Config struct {
	Mode              Mode
	Issuer            string
	Audience          string
	JWKSURL           string
	OIDCDiscoveryURL  string
	AllowedAlgorithms []string
	SubjectClaim      string
	CustomerIDClaim   string
	RoleClaim         string
	RoleCustomerValue string
	RoleAdminValue    string
	RoleSystemValue   string
}

type Middleware func(http.Handler) http.Handler

type discoveryDocument struct {
	Issuer  string `json:"issuer"`
	JWKSURL string `json:"jwks_uri"`
}

func NewIdentityMiddleware(ctx context.Context, cfg Config) (Middleware, error) {
	return newIdentityMiddleware(ctx, http.DefaultClient, cfg)
}

func newIdentityMiddleware(ctx context.Context, client *http.Client, cfg Config) (Middleware, error) {
	switch cfg.Mode {
	case ModeLocked:
		return IdentityMiddleware(ModeLocked), nil
	case ModeDev:
		return IdentityMiddleware(ModeDev), nil
	case ModeJWT:
		return newJWTIdentityMiddleware(ctx, client, cfg)
	default:
		return nil, fmt.Errorf("unsupported auth mode %q", cfg.Mode)
	}
}

func newJWTIdentityMiddleware(ctx context.Context, client *http.Client, cfg Config) (Middleware, error) {
	jwksURL := cfg.JWKSURL
	if cfg.OIDCDiscoveryURL != "" {
		discovery, err := fetchDiscoveryDocument(ctx, client, cfg.OIDCDiscoveryURL)
		if err != nil {
			return nil, fmt.Errorf("load OIDC discovery document: %w", err)
		}
		if discovery.Issuer != cfg.Issuer {
			return nil, errors.New("OIDC discovery issuer does not match OMS_JWT_ISSUER")
		}
		if discovery.JWKSURL == "" {
			return nil, errors.New("OIDC discovery document is missing jwks_uri")
		}
		jwksURL = discovery.JWKSURL
	}
	if err := validateHTTPSURL(jwksURL); err != nil {
		return nil, err
	}

	keySetContext := oidc.ClientContext(ctx, client)
	keySet := oidc.NewRemoteKeySet(keySetContext, jwksURL)
	verifier := oidc.NewVerifier(cfg.Issuer, keySet, &oidc.Config{
		ClientID:             cfg.Audience,
		SupportedSigningAlgs: cfg.AllowedAlgorithms,
	})

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rawToken, err := bearerToken(r.Header.Get("Authorization"))
			if err != nil {
				writeUnauthorized(w)
				return
			}

			token, err := verifier.Verify(r.Context(), rawToken)
			if err != nil {
				writeUnauthorized(w)
				return
			}

			identity, err := identityFromJWT(token, cfg)
			if err != nil {
				writeUnauthorized(w)
				return
			}

			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), identity)))
		})
	}, nil
}

func fetchDiscoveryDocument(ctx context.Context, client *http.Client, discoveryURL string) (discoveryDocument, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return discoveryDocument{}, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return discoveryDocument{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return discoveryDocument{}, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var discovery discoveryDocument
	if err := json.NewDecoder(resp.Body).Decode(&discovery); err != nil {
		return discoveryDocument{}, err
	}

	return discovery, nil
}

func validateHTTPSURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return errors.New("JWKS URL must be a valid HTTPS URL")
	}

	return nil
}

func bearerToken(value string) (string, error) {
	parts := strings.Fields(value)
	if len(parts) != 2 || parts[0] != "Bearer" || parts[1] == "" {
		return "", errors.New("missing or malformed bearer token")
	}

	return parts[1], nil
}

func identityFromJWT(token *oidc.IDToken, cfg Config) (Identity, error) {
	var claims map[string]json.RawMessage
	if err := token.Claims(&claims); err != nil {
		return Identity{}, err
	}

	subject, err := stringClaim(claims, cfg.SubjectClaim)
	if err != nil || strings.TrimSpace(subject) == "" {
		return Identity{}, errors.New("missing subject claim")
	}

	role, err := resolveRole(claims[cfg.RoleClaim], cfg)
	if err != nil {
		return Identity{}, err
	}

	identity := Identity{
		Role:       role,
		Subject:    subject,
		HasSubject: true,
	}

	switch role {
	case RoleCustomer:
		customerIDValue, err := stringClaim(claims, cfg.CustomerIDClaim)
		if err != nil {
			return Identity{}, errors.New("missing customer id claim")
		}

		customerID, err := uuid.Parse(customerIDValue)
		if err != nil {
			return Identity{}, errors.New("invalid customer id claim")
		}

		identity.CustomerID = customerID
		identity.HasCustomerID = true
	case RoleAdmin, RoleSystem:
		subjectID, err := uuid.Parse(subject)
		if err != nil {
			return Identity{}, errors.New("privileged subject claim must be a UUID")
		}

		identity.SubjectID = subjectID
		identity.HasSubjectID = true
	}

	return identity, nil
}

func stringClaim(claims map[string]json.RawMessage, name string) (string, error) {
	raw, ok := claims[name]
	if !ok {
		return "", errors.New("missing claim")
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errors.New("claim must be a string")
	}

	return value, nil
}

func resolveRole(raw json.RawMessage, cfg Config) (Role, error) {
	if len(raw) == 0 {
		return "", errors.New("missing role claim")
	}

	var values []string
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		values = []string{single}
	} else if err := json.Unmarshal(raw, &values); err != nil {
		return "", errors.New("role claim must be a string or string array")
	}

	var matched Role
	for _, value := range values {
		var role Role
		switch value {
		case cfg.RoleCustomerValue:
			role = RoleCustomer
		case cfg.RoleAdminValue:
			role = RoleAdmin
		case cfg.RoleSystemValue:
			role = RoleSystem
		default:
			continue
		}

		if matched != "" && matched != role {
			return "", errors.New("role claim resolves to multiple OMS roles")
		}
		matched = role
	}

	if matched == "" {
		return "", errors.New("role claim does not resolve to an OMS role")
	}

	return matched, nil
}

func writeUnauthorized(w http.ResponseWriter) {
	writeJSONError(w, http.StatusUnauthorized, "unauthorized", http.StatusText(http.StatusUnauthorized))
}
