package secure

import (
	"testing"
	"time"
)

func TestPassword(t *testing.T) {
	h, err := HashPassword("пароль-123")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(h, "пароль-123") || VerifyPassword(h, "пароль-124") || VerifyPassword("garbage", "x") {
		t.Fatal("verify mismatch")
	}
}

// Тестовый вектор RFC 6238 (SHA1, секрет "12345678901234567890").
func TestTOTPVector(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	cases := map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"}
	for ts, want := range cases {
		got, err := TOTPCode(secret, time.Unix(ts, 0))
		if err != nil || got != want {
			t.Fatalf("t=%d: got %s want %s (%v)", ts, got, want, err)
		}
	}
	now := time.Unix(1234567890, 0)
	if !VerifyTOTP(secret, "005924", now) || !VerifyTOTP(secret, "005924", now.Add(30*time.Second)) {
		t.Fatal("window")
	}
	if VerifyTOTP(secret, "005924", now.Add(90*time.Second)) || VerifyTOTP(secret, "12345", now) {
		t.Fatal("should reject")
	}
}

func TestRecovery(t *testing.T) {
	codes := RecoveryCodes(10)
	seen := map[string]bool{}
	for _, c := range codes {
		if len(c) != 9 || c[4] != '-' || seen[c] {
			t.Fatalf("bad code %q", c)
		}
		seen[c] = true
		if NormalizeRecovery(" "+c[:4]+c[5:]+" ") != c {
			t.Fatal("normalize")
		}
	}
}

// VerifyTOTPStep возвращает интервал, к которому подошёл код, — по нему отсекается повтор.
func TestTOTPStep(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	now := time.Unix(1234567890, 0)
	want := int64(1234567890 / 30)
	if step, ok := VerifyTOTPStep(secret, "005924", now); !ok || step != want {
		t.Fatalf("same step: %d %v", step, ok)
	}
	// тот же код через 30 с принимается как предыдущий интервал — шаг остаётся прежним
	if step, ok := VerifyTOTPStep(secret, "005924", now.Add(30*time.Second)); !ok || step != want {
		t.Fatalf("prev step: %d %v", step, ok)
	}
	if _, ok := VerifyTOTPStep(secret, "000000", now); ok {
		t.Fatal("wrong code accepted")
	}
}
