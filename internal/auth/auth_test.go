package auth

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func mustKey(t *testing.T) string {
	t.Helper()
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestAuthenticate(t *testing.T) {
	alice, support := mustKey(t), mustKey(t)
	a, err := NewAuthenticator([]KeyEntry{
		{ID: "alice", KeySHA256: HashKey(alice), Role: RoleCustomer, CustomerID: "7001"},
		{ID: "support-bot", KeySHA256: HashKey(support), Role: RoleSupport},
	})
	if err != nil {
		t.Fatal(err)
	}

	p, err := a.Authenticate(alice)
	if err != nil || p.ID != "alice" || p.Role != RoleCustomer || p.CustomerID != "7001" {
		t.Fatalf("Authenticate(alice) = %+v, %v", p, err)
	}
	if p, err := a.Authenticate(support); err != nil || p.Role != RoleSupport {
		t.Fatalf("Authenticate(support) = %+v, %v", p, err)
	}

	for name, tc := range map[string]struct {
		key  string
		want error
	}{
		"empty":     {"", ErrMissingCredentials},
		"too short": {"og_short", ErrInvalidCredentials},
		"too long":  {strings.Repeat("x", 300), ErrInvalidCredentials},
		"unknown":   {mustKey(t), ErrInvalidCredentials},
	} {
		if _, err := a.Authenticate(tc.key); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestNewAuthenticatorValidation(t *testing.T) {
	h := HashKey("og_0123456789abcdefghij")
	cases := map[string][]KeyEntry{
		"empty":                {},
		"bad id":               {{ID: "a b", KeySHA256: h, Role: RoleSupport}},
		"bad digest":           {{ID: "a", KeySHA256: "abc", Role: RoleSupport}},
		"unknown role":         {{ID: "a", KeySHA256: h, Role: "admin"}},
		"customer without id":  {{ID: "a", KeySHA256: h, Role: RoleCustomer}},
		"support with id":      {{ID: "a", KeySHA256: h, Role: RoleSupport, CustomerID: "1"}},
		"duplicate id":         {{ID: "a", KeySHA256: h, Role: RoleSupport}, {ID: "a", KeySHA256: HashKey("other-key-0123456789"), Role: RoleSupport}},
		"duplicate digest":     {{ID: "a", KeySHA256: h, Role: RoleSupport}, {ID: "b", KeySHA256: h, Role: RoleSupport}},
		"non-numeric customer": {{ID: "a", KeySHA256: h, Role: RoleCustomer, CustomerID: "abc"}},
	}
	for name, entries := range cases {
		if _, err := NewAuthenticator(entries); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseKeysRejectsUnknownFields(t *testing.T) {
	_, err := ParseKeys(strings.NewReader(`[{"id":"a","key":"plaintext"}]`))
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
}

func TestBearerToken(t *testing.T) {
	for header, want := range map[string]error{
		"":                   ErrMissingCredentials,
		"Bearer abc":         nil,
		"bearer abc":         nil,
		"Basic dXNlcjpwYXNz": ErrInvalidCredentials,
		"Bearer ":            ErrInvalidCredentials,
		"Bearerabc":          ErrInvalidCredentials,
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		tok, err := BearerToken(r)
		if !errors.Is(err, want) {
			t.Errorf("%q: err = %v, want %v", header, err, want)
		}
		if want == nil && tok != "abc" {
			t.Errorf("%q: token = %q", header, tok)
		}
	}
}

func TestCanAccessOrder(t *testing.T) {
	alice := Principal{ID: "alice", Role: RoleCustomer, CustomerID: "7001"}
	support := Principal{ID: "s", Role: RoleSupport}
	cases := []struct {
		p     Principal
		owner string
		want  bool
	}{
		{alice, "7001", true},
		{alice, "7002", false},
		{alice, "", false}, // guest order
		{support, "7002", true},
		{support, "", true},
		{Principal{ID: "x", Role: "unknown"}, "7001", false},
		{Principal{ID: "x", Role: RoleCustomer}, "", false},
	}
	for _, c := range cases {
		if got := CanAccessOrder(c.p, c.owner); got != c.want {
			t.Errorf("CanAccessOrder(%+v, %q) = %v, want %v", c.p, c.owner, got, c.want)
		}
	}
}

func TestGenerateKeyIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		k := mustKey(t)
		if seen[k] || !strings.HasPrefix(k, "og_") || len(k) < minKeyLen {
			t.Fatalf("bad or duplicate key %q", k)
		}
		seen[k] = true
	}
}
