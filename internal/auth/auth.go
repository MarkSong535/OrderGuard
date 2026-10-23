// Package auth implements API-key authentication and order-level
// authorization.
//
// Keys are configured only as SHA-256 digests, so neither the configuration
// nor a memory dump of the key table reveals a usable credential.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// Role determines what a principal may access.
type Role string

const (
	// RoleCustomer may read only orders that belong to its customer ID.
	RoleCustomer Role = "customer"
	// RoleSupport may read any order (support staff, trusted AI support agents).
	RoleSupport Role = "support"
)

// Principal is an authenticated caller.
type Principal struct {
	ID         string // stable, non-secret key identifier used in logs
	Role       Role
	CustomerID string // set for RoleCustomer
}

// KeyEntry is one configured API key.
type KeyEntry struct {
	ID         string `json:"id"`
	KeySHA256  string `json:"key_sha256"`
	Role       Role   `json:"role"`
	CustomerID string `json:"customer_id,omitempty"`
}

var (
	// ErrMissingCredentials means no bearer token was presented.
	ErrMissingCredentials = errors.New("missing credentials")
	// ErrInvalidCredentials means the token was malformed or unknown.
	ErrInvalidCredentials = errors.New("invalid credentials")
)

const (
	minKeyLen = 20
	maxKeyLen = 256
	keyPrefix = "og_"
)

var (
	idPattern       = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	hexDigest       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	customerPattern = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
)

// Authenticator resolves bearer tokens to principals.
type Authenticator struct {
	keys map[[sha256.Size]byte]Principal
}

// ParseKeys decodes a JSON array of KeyEntry.
func ParseKeys(r io.Reader) ([]KeyEntry, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var entries []KeyEntry
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("parse API keys: %w", err)
	}
	return entries, nil
}

// NewAuthenticator validates entries and builds the key table.
func NewAuthenticator(entries []KeyEntry) (*Authenticator, error) {
	if len(entries) == 0 {
		return nil, errors.New("no API keys configured")
	}
	a := &Authenticator{keys: make(map[[sha256.Size]byte]Principal, len(entries))}
	seen := make(map[string]bool, len(entries))
	for i, e := range entries {
		if !idPattern.MatchString(e.ID) {
			return nil, fmt.Errorf("key %d: invalid id %q", i, e.ID)
		}
		if seen[e.ID] {
			return nil, fmt.Errorf("key %q: duplicate id", e.ID)
		}
		seen[e.ID] = true

		digest := strings.ToLower(e.KeySHA256)
		if !hexDigest.MatchString(digest) {
			return nil, fmt.Errorf("key %q: key_sha256 must be 64 hex characters", e.ID)
		}
		var h [sha256.Size]byte
		_, _ = hex.Decode(h[:], []byte(digest))
		if _, dup := a.keys[h]; dup {
			return nil, fmt.Errorf("key %q: duplicate key digest", e.ID)
		}

		switch e.Role {
		case RoleCustomer:
			if !customerPattern.MatchString(e.CustomerID) {
				return nil, fmt.Errorf("key %q: customer role requires a numeric customer_id", e.ID)
			}
		case RoleSupport:
			if e.CustomerID != "" {
				return nil, fmt.Errorf("key %q: support role must not set customer_id", e.ID)
			}
		default:
			return nil, fmt.Errorf("key %q: unknown role %q", e.ID, e.Role)
		}
		a.keys[h] = Principal{ID: e.ID, Role: e.Role, CustomerID: e.CustomerID}
	}
	return a, nil
}

// Authenticate resolves a raw API key. Lookup is by SHA-256 digest, so the
// comparison never branches on secret bytes.
func (a *Authenticator) Authenticate(key string) (Principal, error) {
	if key == "" {
		return Principal{}, ErrMissingCredentials
	}
	if len(key) < minKeyLen || len(key) > maxKeyLen {
		return Principal{}, ErrInvalidCredentials
	}
	p, ok := a.keys[sha256.Sum256([]byte(key))]
	if !ok {
		return Principal{}, ErrInvalidCredentials
	}
	return p, nil
}

// BearerToken extracts the token from an "Authorization: Bearer" header.
func BearerToken(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", ErrMissingCredentials
	}
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", ErrInvalidCredentials
	}
	return strings.TrimSpace(token), nil
}

// HashKey returns the hex SHA-256 digest to store in configuration.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// GenerateKey returns a new random API key with 256 bits of entropy.
func GenerateKey() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return keyPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// CanAccessOrder is the order-level authorization policy.
func CanAccessOrder(p Principal, orderCustomerID string) bool {
	switch p.Role {
	case RoleSupport:
		return true
	case RoleCustomer:
		// Guest orders (no customer) are never visible to customer keys.
		return p.CustomerID != "" && orderCustomerID != "" &&
			subtle.ConstantTimeCompare([]byte(p.CustomerID), []byte(orderCustomerID)) == 1
	}
	return false
}

type principalKey struct{}

// WithPrincipal stores p in ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal stored in ctx.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
