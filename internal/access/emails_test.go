package access

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type emailFixture struct {
	key       *rsa.PrivateKey
	issuer    string
	validator *Validator
}

func newEmailFixture(t *testing.T) *emailFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jwksDocument{Keys: []jwk{{
			KeyType: "RSA",
			KeyID:   "key-1",
			N:       base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			E:       base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(server.Close)
	validator, err := NewValidator(server.URL, "aud")
	if err != nil {
		t.Fatal(err)
	}
	return &emailFixture{key: key, issuer: server.URL, validator: validator}
}

func (f *emailFixture) sign(t *testing.T, key *rsa.PrivateKey, email string) string {
	t.Helper()
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    f.issuer,
			Audience:  jwt.ClaimStrings{"aud"},
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
		Email: email,
	})
	token.Header["kid"] = "key-1"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestRestrictEmailsAcceptsListedAddressOnly(t *testing.T) {
	fixture := newEmailFixture(t)
	if err := fixture.validator.RestrictEmails([]string{" Marvin@Example.com ", "b@example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"marvin@example.com", "MARVIN@EXAMPLE.COM", " b@example.com "} {
		if err := fixture.validator.Validate(context.Background(), fixture.sign(t, fixture.key, email)); err != nil {
			t.Fatalf("%q rejected: %v", email, err)
		}
	}
	for email, reason := range map[string]string{
		"":                  "email claim missing",
		"   ":               "email claim missing",
		"other@example.com": "not in the allow-list",
	} {
		err := fixture.validator.Validate(context.Background(), fixture.sign(t, fixture.key, email))
		if err == nil || !strings.Contains(err.Error(), reason) {
			t.Fatalf("%q: err = %v, want %q", email, err, reason)
		}
	}
}

func TestRestrictEmailsStillVerifiesTheSignature(t *testing.T) {
	fixture := newEmailFixture(t)
	if err := fixture.validator.RestrictEmails([]string{"a@example.com"}); err != nil {
		t.Fatal(err)
	}
	forger, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.validator.Validate(context.Background(), fixture.sign(t, forger, "a@example.com")); err == nil {
		t.Fatal("forged assertion with a listed email was accepted")
	}
}

func TestRestrictEmailsRejectsEmptyList(t *testing.T) {
	validator := &Validator{}
	for _, emails := range [][]string{nil, {}, {""}, {" ", ""}} {
		if err := validator.RestrictEmails(emails); err == nil {
			t.Fatalf("RestrictEmails(%v) succeeded", emails)
		}
		if validator.restrictEmails {
			t.Fatalf("RestrictEmails(%v) switched the check on with no addresses", emails)
		}
	}
}

func TestWithoutRestrictionEmailIsIgnored(t *testing.T) {
	fixture := newEmailFixture(t)
	if err := fixture.validator.Validate(context.Background(), fixture.sign(t, fixture.key, "")); err != nil {
		t.Fatal(err)
	}
}

func TestMiddlewareLogsReasonButNeverTheToken(t *testing.T) {
	fixture := newEmailFixture(t)
	if err := fixture.validator.RestrictEmails([]string{"a@example.com"}); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	called := false
	handler := fixture.validator.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	assertion := fixture.sign(t, fixture.key, "")
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set(assertionHeader, assertion)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if called || response.Code != http.StatusUnauthorized {
		t.Fatalf("called = %v, status = %d", called, response.Code)
	}
	if !strings.Contains(logged.String(), "email claim missing") {
		t.Fatalf("log = %q", logged.String())
	}
	if strings.Contains(logged.String(), assertion) || strings.Contains(response.Body.String(), assertion) {
		t.Fatal("the assertion was logged or echoed")
	}
}
