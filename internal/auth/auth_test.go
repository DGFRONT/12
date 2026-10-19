package auth

import (
	"testing"
	"time"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("s3cret-пароль")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := CheckPassword(h, "s3cret-пароль"); err != nil || !ok {
		t.Fatalf("correct password rejected: ok=%v err=%v", ok, err)
	}
	if ok, _ := CheckPassword(h, "wrong"); ok {
		t.Fatal("wrong password accepted")
	}
	if _, err := CheckPassword("garbage", "x"); err == nil {
		t.Fatal("expected error for malformed hash")
	}
}

func TestAccessToken(t *testing.T) {
	tk := NewTokens([]byte("0123456789abcdef0123456789abcdef"))
	s, _, err := tk.IssueAccess("user-1", "admin")
	if err != nil {
		t.Fatal(err)
	}
	c, err := tk.ParseAccess(s)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "user-1" || c.Role != "admin" {
		t.Fatalf("unexpected claims %+v", c)
	}

	other := NewTokens([]byte("another-secret-another-secret-xx"))
	if _, err := other.ParseAccess(s); err == nil {
		t.Fatal("token signed with another secret accepted")
	}

	tk.now = func() time.Time { return time.Now().Add(AccessTTL + time.Minute) }
	if _, err := tk.ParseAccess(s); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestLoginLimiter(t *testing.T) {
	now := time.Now()
	l := NewLoginLimiter(3, time.Minute)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if !l.Allowed("k") {
			t.Fatalf("attempt %d blocked too early", i)
		}
		l.Fail("k")
	}
	if l.Allowed("k") {
		t.Fatal("expected block after 3 failures")
	}
	now = now.Add(2 * time.Minute)
	if !l.Allowed("k") {
		t.Fatal("expected unblock after window")
	}
}

func TestGenerators(t *testing.T) {
	s := GenerateSlug()
	if len(s) != 12 {
		t.Fatalf("slug length %d", len(s))
	}
	if GenerateSlug() == s {
		t.Fatal("slugs repeat")
	}
	if len(GeneratePassword(14)) != 14 {
		t.Fatal("password length")
	}
}

func TestGuestToken(t *testing.T) {
	tk := NewTokens([]byte("0123456789abcdef0123456789abcdef"))
	s, err := tk.IssueGuest("g1", "Гость", "room123")
	if err != nil {
		t.Fatal(err)
	}
	c, err := tk.ParseGuest(s)
	if err != nil || c.Subject != "g1" || c.Room != "room123" || c.Name != "Гость" {
		t.Fatalf("claims %+v err %v", c, err)
	}
	if _, err := tk.ParseAccess(s); err == nil {
		t.Fatal("guest token accepted as access token")
	}
	access, _, _ := tk.IssueAccess("u1", "user")
	if _, err := tk.ParseGuest(access); err == nil {
		t.Fatal("access token accepted as guest token")
	}
}
