package crypto

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
)

func TestArgon2HashVerify(t *testing.T) {
	h := NewArgon2Hasher(TestParams())
	encoded, err := h.Hash("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$") {
		t.Fatalf("unexpected PHC encoding: %s", encoded)
	}

	ok, _, err := h.Verify("correct-horse-battery-staple", encoded)
	if err != nil || !ok {
		t.Fatalf("correct password rejected: ok=%v err=%v", ok, err)
	}
	ok, _, err = h.Verify("wrong-password", encoded)
	if err != nil {
		t.Fatalf("verify errored on wrong password: %v", err)
	}
	if ok {
		t.Fatal("wrong password accepted")
	}
}

func TestArgon2SaltsDiffer(t *testing.T) {
	h := NewArgon2Hasher(TestParams())
	a, _ := h.Hash("same-password-here")
	b, _ := h.Hash("same-password-here")
	if a == b {
		t.Fatal("identical hashes for the same password: salt is not random")
	}
}

func TestArgon2NeedsRehashOnParamUpgrade(t *testing.T) {
	weak := NewArgon2Hasher(Argon2Params{Time: 1, Memory: 8 * 1024, Threads: 1, KeyLen: 32, SaltLen: 16})
	encoded, _ := weak.Hash("upgrade-me-please")

	strong := NewArgon2Hasher(Argon2Params{Time: 3, Memory: 16 * 1024, Threads: 1, KeyLen: 32, SaltLen: 16})
	ok, needsRehash, err := strong.Verify("upgrade-me-please", encoded)
	if err != nil || !ok {
		t.Fatalf("verify failed: ok=%v err=%v", ok, err)
	}
	if !needsRehash {
		t.Fatal("weaker stored parameters should request a rehash")
	}
}

func TestArgon2RejectsMalformedHash(t *testing.T) {
	h := NewArgon2Hasher(TestParams())
	for _, bad := range []string{"", "plaintext", "$argon2i$v=19$m=8,t=1,p=1$AAAA$AAAA", "$argon2id$v=99$m=8,t=1,p=1$AAAA$AAAA"} {
		if _, _, err := h.Verify("x", bad); err == nil {
			t.Fatalf("expected error for malformed hash %q", bad)
		}
	}
}

func TestJWTRoundTrip(t *testing.T) {
	j := NewJWTIssuer(strings.Repeat("s", 32), nil)
	p := domain.Principal{UserID: domain.NewID(), Email: "dev@genesis.local", Role: domain.RoleOwner}

	tok, exp, err := j.Issue(p, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !exp.After(time.Now()) {
		t.Fatal("expiry should be in the future")
	}
	got, err := j.Parse(tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != p {
		t.Fatalf("principal round-trip mismatch: %+v vs %+v", got, p)
	}
}

func TestJWTRejectsTampering(t *testing.T) {
	j := NewJWTIssuer(strings.Repeat("s", 32), nil)
	p := domain.Principal{UserID: domain.NewID(), Email: "a@b.co", Role: domain.RoleViewer}
	tok, _, _ := j.Issue(p, time.Hour)

	parts := strings.Split(tok, ".")

	// Payload swapped for an escalated role, signature left intact.
	forged := parts[0] + "." + b64([]byte(`{"sub":"`+p.UserID.String()+`","role":"owner","iss":"genesis-ai-factory","aud":"genesis-desktop","exp":9999999999,"nbf":0}`)) + "." + parts[2]
	if _, err := j.Parse(forged); err == nil {
		t.Fatal("forged payload accepted")
	}

	// alg:none attack.
	none := b64([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + parts[1] + "."
	if _, err := j.Parse(none); err == nil {
		t.Fatal("unsigned token accepted")
	}

	// Different secret.
	other := NewJWTIssuer(strings.Repeat("x", 32), nil)
	if _, err := other.Parse(tok); err == nil {
		t.Fatal("token verified under the wrong secret")
	}
}

func TestJWTExpiryAndSkew(t *testing.T) {
	base := time.Now()
	clock := base
	j := NewJWTIssuer(strings.Repeat("s", 32), func() time.Time { return clock })
	p := domain.Principal{UserID: domain.NewID(), Email: "a@b.co", Role: domain.RoleMember}

	tok, _, _ := j.Issue(p, time.Minute)
	clock = base.Add(2 * time.Minute)
	_, err := j.Parse(tok)
	if err == nil {
		t.Fatal("expired token accepted")
	}
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("expected unauthorized sentinel, got %v", err)
	}
}

func TestOpaqueTokenHashing(t *testing.T) {
	tok, hash, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(tok) < 40 {
		t.Fatalf("token too short: %d chars", len(tok))
	}
	if HashToken(tok) != hash {
		t.Fatal("hash is not reproducible")
	}
	if strings.Contains(hash, tok) {
		t.Fatal("hash leaks the token")
	}
	tok2, hash2, _ := NewOpaqueToken()
	if tok == tok2 || hash == hash2 {
		t.Fatal("tokens are not unique")
	}
}
