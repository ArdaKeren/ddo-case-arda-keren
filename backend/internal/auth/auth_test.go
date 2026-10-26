package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func TestPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery", DefaultArgon2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected hash format: %s", hash)
	}
	ok, rehash, err := VerifyPassword("correct horse battery", hash, DefaultArgon2)
	if err != nil || !ok || rehash {
		t.Fatalf("verify correct: ok=%v rehash=%v err=%v", ok, rehash, err)
	}
	if ok, _, _ := VerifyPassword("wrong password!!", hash, DefaultArgon2); ok {
		t.Fatal("wrong password accepted")
	}
}

func TestPasswordNeedsRehash(t *testing.T) {
	weak := Argon2Params{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	hash, _ := HashPassword("correct horse battery", weak)
	ok, rehash, err := VerifyPassword("correct horse battery", hash, DefaultArgon2)
	if err != nil || !ok || !rehash {
		t.Fatalf("ok=%v rehash=%v err=%v", ok, rehash, err)
	}
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	for _, h := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$AA$AA", "$argon2id$v=19$bad$AA$AA"} {
		if _, _, err := VerifyPassword("x", h, DefaultArgon2); err == nil {
			t.Errorf("expected error for %q", h)
		}
	}
}

func TestJWTRoundTrip(t *testing.T) {
	ti := NewTokenIssuer(testSecret, "qa-test", time.Minute)
	tok, _, err := ti.Issue("user-1", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ti.Parse(tok)
	if err != nil || c.Subject != "user-1" || c.Role != RoleAdmin {
		t.Fatalf("claims=%+v err=%v", c, err)
	}
}

func TestJWTRejections(t *testing.T) {
	ti := NewTokenIssuer(testSecret, "qa-test", time.Minute)

	other := NewTokenIssuer([]byte("ffffffffffffffffffffffffffffffff"), "qa-test", time.Minute)
	forged, _, _ := other.Issue("user-1", RoleAdmin)
	if _, err := ti.Parse(forged); err == nil {
		t.Error("token signed with another key accepted")
	}

	expired, _, _ := NewTokenIssuer(testSecret, "qa-test", -time.Hour).Issue("user-1", RoleUser)
	if _, err := ti.Parse(expired); err == nil {
		t.Error("expired token accepted")
	}

	wrongIss, _, _ := NewTokenIssuer(testSecret, "someone-else", time.Minute).Issue("user-1", RoleUser)
	if _, err := ti.Parse(wrongIss); err == nil {
		t.Error("token with wrong issuer accepted")
	}

	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, Claims{Role: RoleAdmin, RegisteredClaims: jwt.RegisteredClaims{
		Subject: "user-1", Issuer: "qa-test", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
	}}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if _, err := ti.Parse(none); err == nil {
		t.Error(`alg "none" token accepted`)
	}

	badRole, _, _ := ti.Issue("user-1", Role("superuser"))
	if _, err := ti.Parse(badRole); err == nil {
		t.Error("token with unknown role accepted")
	}
}

func TestRBAC(t *testing.T) {
	if !RoleAdmin.Can(PermUsersWrite) || !RoleAdmin.Can(PermAuditRead) {
		t.Error("admin missing permissions")
	}
	if RoleUser.Can(PermUsersRead) || RoleUser.Can(PermAuditRead) {
		t.Error("user has admin permissions")
	}
	if !RoleUser.Can(PermProfileRead) {
		t.Error("user cannot read own profile")
	}
	if Role("ghost").Valid() || Role("ghost").Can(PermProfileRead) {
		t.Error("unknown role treated as valid")
	}
}

func TestMiddlewareChain(t *testing.T) {
	ti := NewTokenIssuer(testSecret, "qa-test", time.Minute)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := Authenticate(ti)(RequirePermission(PermUsersRead)(ok))

	userTok, _, _ := ti.Issue("u", RoleUser)
	adminTok, _, _ := ti.Issue("a", RoleAdmin)

	cases := []struct {
		name, header string
		want         int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"garbage", "Bearer nope", http.StatusUnauthorized},
		{"wrong scheme", "Basic " + adminTok, http.StatusUnauthorized},
		{"user lacks perm", "Bearer " + userTok, http.StatusForbidden},
		{"admin allowed", "Bearer " + adminTok, http.StatusOK},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if c.header != "" {
			req.Header.Set("Authorization", c.header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, rec.Code, c.want)
		}
	}

	roleOnly := Authenticate(ti)(RequireRole(RoleAdmin)(ok))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+userTok)
	rec := httptest.NewRecorder()
	roleOnly.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("RequireRole: got %d, want 403", rec.Code)
	}
}

func TestNormalizeEmail(t *testing.T) {
	if e, ok := NormalizeEmail("  Ali@Example.COM "); !ok || e != "ali@example.com" {
		t.Errorf("got %q %v", e, ok)
	}
	for _, bad := range []string{"", "not-an-email", "Ali <ali@example.com>"} {
		if _, ok := NormalizeEmail(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}
