package platform

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"oms/internal/auth"
)

const (
	defaultHTTPAddr        = ":8080"
	defaultShutdownTimeout = 10 * time.Second
	defaultWorkerBatchSize = int32(500)
	defaultReadTimeout     = 5 * time.Second
	defaultWriteTimeout    = 10 * time.Second
	defaultIdleTimeout     = 60 * time.Second
)

type Config struct {
	HTTPAddr        string
	ShutdownTimeout time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	DatabaseURL     string
	WorkerBatchSize int32
	AuthMode        auth.Mode
	AuthConfig      auth.Config
}

func LoadConfig() (Config, error) {
	cfg := Config{
		HTTPAddr:        defaultHTTPAddr,
		ShutdownTimeout: defaultShutdownTimeout,
		WorkerBatchSize: defaultWorkerBatchSize,
		ReadTimeout:     defaultReadTimeout,
		WriteTimeout:    defaultWriteTimeout,
		IdleTimeout:     defaultIdleTimeout,
	}

	if httpAddr := os.Getenv("HTTP_ADDR"); httpAddr != "" {
		cfg.HTTPAddr = httpAddr
	}

	if shutdownTimeout := os.Getenv("SHUTDOWN_TIMEOUT"); shutdownTimeout != "" {
		if parsed, err := time.ParseDuration(shutdownTimeout); err == nil {
			cfg.ShutdownTimeout = parsed
		}
	}

	readTimeout, err := parsePositiveDurationEnv("HTTP_READ_TIMEOUT", cfg.ReadTimeout)
	if err != nil {
		return Config{}, err
	}
	cfg.ReadTimeout = readTimeout

	writeTimeout, err := parsePositiveDurationEnv("HTTP_WRITE_TIMEOUT", cfg.WriteTimeout)
	if err != nil {
		return Config{}, err
	}
	cfg.WriteTimeout = writeTimeout

	idleTimeout, err := parsePositiveDurationEnv("HTTP_IDLE_TIMEOUT", cfg.IdleTimeout)
	if err != nil {
		return Config{}, err
	}
	cfg.IdleTimeout = idleTimeout

	cfg.DatabaseURL = os.Getenv("DATABASE_URL")
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}

	if workerBatchSize := os.Getenv("WORKER_BATCH_SIZE"); workerBatchSize != "" {
		parsed, err := strconv.Atoi(workerBatchSize)
		if err != nil || parsed < 1 {
			return Config{}, errors.New("WORKER_BATCH_SIZE must be a positive integer")
		}

		cfg.WorkerBatchSize = int32(parsed)
	}

	switch authMode := os.Getenv("OMS_AUTH_MODE"); authMode {
	case "":
		cfg.AuthMode = auth.ModeLocked
	case string(auth.ModeDev):
		cfg.AuthMode = auth.ModeDev
	case string(auth.ModeJWT):
		cfg.AuthMode = auth.ModeJWT
	default:
		return Config{}, fmt.Errorf("OMS_AUTH_MODE must be empty, %q, or %q", auth.ModeDev, auth.ModeJWT)
	}

	cfg.AuthConfig, err = loadAuthConfig(cfg.AuthMode)
	if err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func parsePositiveDurationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}

	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid duration", name)
	}

	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}

	return parsed, nil
}

func loadAuthConfig(mode auth.Mode) (auth.Config, error) {
	cfg := auth.Config{Mode: mode}
	if mode != auth.ModeJWT {
		return cfg, nil
	}

	cfg.Issuer = envTrimmed("OMS_JWT_ISSUER")
	cfg.Audience = envTrimmed("OMS_JWT_AUDIENCE")
	cfg.JWKSURL = envTrimmed("OMS_JWT_JWKS_URL")
	cfg.OIDCDiscoveryURL = envTrimmed("OMS_OIDC_DISCOVERY_URL")
	cfg.SubjectClaim = envTrimmed("OMS_JWT_SUBJECT_CLAIM")
	if cfg.SubjectClaim == "" {
		cfg.SubjectClaim = "sub"
	}
	cfg.CustomerIDClaim = envTrimmed("OMS_JWT_CUSTOMER_ID_CLAIM")
	cfg.RoleClaim = envTrimmed("OMS_JWT_ROLE_CLAIM")
	cfg.RoleCustomerValue = envTrimmed("OMS_JWT_ROLE_CUSTOMER_VALUE")
	cfg.RoleAdminValue = envTrimmed("OMS_JWT_ROLE_ADMIN_VALUE")
	cfg.RoleSystemValue = envTrimmed("OMS_JWT_ROLE_SYSTEM_VALUE")

	for name, value := range map[string]string{
		"OMS_JWT_ISSUER":              cfg.Issuer,
		"OMS_JWT_AUDIENCE":            cfg.Audience,
		"OMS_JWT_CUSTOMER_ID_CLAIM":   cfg.CustomerIDClaim,
		"OMS_JWT_ROLE_CLAIM":          cfg.RoleClaim,
		"OMS_JWT_ROLE_CUSTOMER_VALUE": cfg.RoleCustomerValue,
		"OMS_JWT_ROLE_ADMIN_VALUE":    cfg.RoleAdminValue,
		"OMS_JWT_ROLE_SYSTEM_VALUE":   cfg.RoleSystemValue,
	} {
		if strings.TrimSpace(value) == "" {
			return auth.Config{}, fmt.Errorf("%s is required when OMS_AUTH_MODE=%q", name, auth.ModeJWT)
		}
	}

	if (cfg.JWKSURL == "") == (cfg.OIDCDiscoveryURL == "") {
		return auth.Config{}, errors.New("exactly one of OMS_JWT_JWKS_URL or OMS_OIDC_DISCOVERY_URL is required when OMS_AUTH_MODE=\"jwt\"")
	}

	for name, value := range map[string]string{
		"OMS_JWT_ISSUER":         cfg.Issuer,
		"OMS_JWT_JWKS_URL":       cfg.JWKSURL,
		"OMS_OIDC_DISCOVERY_URL": cfg.OIDCDiscoveryURL,
	} {
		if value != "" {
			if err := validateHTTPSURL(name, value); err != nil {
				return auth.Config{}, err
			}
		}
	}

	if cfg.RoleCustomerValue == cfg.RoleAdminValue || cfg.RoleCustomerValue == cfg.RoleSystemValue || cfg.RoleAdminValue == cfg.RoleSystemValue {
		return auth.Config{}, errors.New("OMS JWT role values must be distinct")
	}

	allowedAlgorithms, err := parseAllowedAlgorithms(os.Getenv("OMS_JWT_ALLOWED_ALGS"))
	if err != nil {
		return auth.Config{}, err
	}
	cfg.AllowedAlgorithms = allowedAlgorithms

	return cfg, nil
}

func envTrimmed(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

func validateHTTPSURL(name, value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return fmt.Errorf("%s must be a valid HTTPS URL", name)
	}

	return nil
}

func parseAllowedAlgorithms(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("OMS_JWT_ALLOWED_ALGS is required when OMS_AUTH_MODE=\"jwt\"")
	}

	parts := strings.Split(value, ",")
	algorithms := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		algorithm := strings.TrimSpace(part)
		if algorithm == "" {
			return nil, errors.New("OMS_JWT_ALLOWED_ALGS must not contain empty values")
		}
		if strings.EqualFold(algorithm, "none") || strings.HasPrefix(strings.ToUpper(algorithm), "HS") {
			return nil, fmt.Errorf("OMS_JWT_ALLOWED_ALGS contains unsupported algorithm %q", algorithm)
		}
		if _, ok := seen[algorithm]; ok {
			continue
		}
		seen[algorithm] = struct{}{}
		algorithms = append(algorithms, algorithm)
	}

	return algorithms, nil
}
