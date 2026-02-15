package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/MicahParks/keyfunc/v2"
	"github.com/golang-jwt/jwt/v5"
)

// KeycloakClaims represents the relevant claims from a Keycloak access token.
type KeycloakClaims struct {
	PreferredUsername string   `json:"preferred_username"`
	Email             string   `json:"email"`
	RealmRoles        []string `json:"-"` // extracted from realm_access
	ExpiresAt         int64    `json:"-"`
}

// RealmAccess is the nested structure in Keycloak tokens.
type RealmAccess struct {
	Roles []string `json:"roles"`
}

// keycloakTokenClaims extends jwt.RegisteredClaims with Keycloak-specific fields.
type keycloakTokenClaims struct {
	jwt.RegisteredClaims
	PreferredUsername string      `json:"preferred_username"`
	Email             string      `json:"email"`
	EmailVerified     bool        `json:"email_verified"`
	RealmAccessField  RealmAccess `json:"realm_access"`
	Scope             string      `json:"scope"`
	Azp               string      `json:"azp"`
}

// KeycloakValidator validates Keycloak JWTs using JWKS.
// It supports lazy initialization: the JWKS keys are fetched on the first
// call to ValidateToken rather than at construction time. This allows the
// auth-service to subscribe to the NATS auth callout subject immediately
// and defer the (potentially slow) Keycloak dependency.
type KeycloakValidator struct {
	jwks      *keyfunc.JWKS
	issuerURL string
	jwksURL   string

	once    sync.Once
	initErr error
}

// NewKeycloakValidator creates a new validator. It does NOT fetch JWKS keys
// at construction time. Keys are lazily fetched on the first ValidateToken call.
// If issuerOverride is non-empty, it is used as the expected token issuer instead
// of deriving it from keycloakURL (needed when the browser-facing URL differs from
// the internal Docker service URL).
func NewKeycloakValidator(keycloakURL, realm, issuerOverride string) *KeycloakValidator {
	jwksURL := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/certs", keycloakURL, realm)
	issuerURL := fmt.Sprintf("%s/realms/%s", keycloakURL, realm)
	if issuerOverride != "" {
		issuerURL = issuerOverride
	}

	slog.Info("Keycloak validator created (lazy init)", "jwks_url", jwksURL, "issuer", issuerURL)

	return &KeycloakValidator{
		issuerURL: issuerURL,
		jwksURL:   jwksURL,
	}
}

// ensureInitialized fetches the JWKS keys on first call (with retries).
// Subsequent calls are no-ops thanks to sync.Once.
func (v *KeycloakValidator) ensureInitialized() error {
	v.once.Do(func() {
		slog.Info("Initializing Keycloak JWKS validator", "jwks_url", v.jwksURL)

		var jwks *keyfunc.JWKS
		var err error
		for attempt := 1; attempt <= 30; attempt++ {
			jwks, err = keyfunc.Get(v.jwksURL, keyfunc.Options{
				Ctx:                 context.Background(),
				RefreshInterval:     5 * time.Minute,
				RefreshRateLimit:    1 * time.Minute,
				RefreshUnknownKID:   true,
				RefreshErrorHandler: func(err error) { slog.Error("JWKS refresh error", "error", err) },
			})
			if err == nil {
				break
			}
			slog.Info("Waiting for Keycloak JWKS", "attempt", attempt, "error", err)
			time.Sleep(2 * time.Second)
		}
		if err != nil {
			v.initErr = fmt.Errorf("failed to fetch Keycloak JWKS after retries: %w", err)
			return
		}

		v.jwks = jwks
		slog.Info("Keycloak JWKS loaded successfully", "jwks_url", v.jwksURL)
	})
	return v.initErr
}

// ValidateToken parses and validates a Keycloak access token JWT.
// On the first call, it lazily initializes the JWKS key set.
func (v *KeycloakValidator) ValidateToken(tokenString string) (*KeycloakClaims, error) {
	if err := v.ensureInitialized(); err != nil {
		return nil, fmt.Errorf("keycloak validator not ready: %w", err)
	}

	claims := &keycloakTokenClaims{}

	token, err := jwt.ParseWithClaims(tokenString, claims, v.jwks.Keyfunc,
		jwt.WithIssuer(v.issuerURL),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("token validation failed: %w", err)
	}

	if !token.Valid {
		return nil, fmt.Errorf("token is not valid")
	}

	// Extract expiration
	var expiresAt int64
	if claims.ExpiresAt != nil {
		expiresAt = claims.ExpiresAt.Unix()
	}

	return &KeycloakClaims{
		PreferredUsername: claims.PreferredUsername,
		Email:             claims.Email,
		RealmRoles:        claims.RealmAccessField.Roles,
		ExpiresAt:         expiresAt,
	}, nil
}

// Close shuts down the JWKS background goroutine.
func (v *KeycloakValidator) Close() {
	if v.jwks != nil {
		v.jwks.EndBackground()
	}
}
