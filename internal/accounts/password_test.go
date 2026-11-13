package accounts

import (
	"strings"
	"testing"
)

// cheap keeps the tests fast; the cost of the real parameters is not what they check.
var cheap = HashParams{Memory: 8, Iterations: 1, Parallelism: 1}

func TestAHashChecksItsOwnPasswordAndNoOther(t *testing.T) {
	h, err := hashPassword("correct horse battery", cheap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=8,t=1,p=1$") {
		t.Fatalf("hash %q is not a PHC string of argon2id", h)
	}
	ok, rehash, err := verifyPassword("correct horse battery", h, cheap)
	if err != nil || !ok || rehash {
		t.Fatalf("the right password: ok=%v rehash=%v err=%v", ok, rehash, err)
	}
	for _, wrong := range []string{"", "correct horse batter", "Correct horse battery", "correct horse battery "} {
		if ok, _, err := verifyPassword(wrong, h, cheap); err != nil || ok {
			t.Errorf("%q was accepted (%v, %v)", wrong, ok, err)
		}
	}
}

func TestTwoHashesOfOnePasswordDiffer(t *testing.T) {
	a, _ := hashPassword("same password here", cheap)
	b, _ := hashPassword("same password here", cheap)
	if a == b {
		t.Fatal("the salt is not random: two hashes of one password are equal")
	}
}

func TestAWeakerHashIsReportedForRehashing(t *testing.T) {
	h, _ := hashPassword("some long password", cheap)
	ok, rehash, err := verifyPassword("some long password", h, HashParams{Memory: 16, Iterations: 1, Parallelism: 1})
	if err != nil || !ok || !rehash {
		t.Fatalf("ok=%v rehash=%v err=%v: a hash made with less memory than now should be renewed", ok, rehash, err)
	}
	// A wrong password never asks for a rehash: it says nothing about the hash.
	if _, rehash, _ := verifyPassword("not it", h, HashParams{Memory: 16, Iterations: 1, Parallelism: 1}); rehash {
		t.Fatal("a rehash was asked for after a wrong password")
	}
}

func TestAStoredHashThatIsGarbageOrHostileIsRefusedNotTrusted(t *testing.T) {
	good, _ := hashPassword("some long password", cheap)
	bad := []string{
		"", "plain text", "$argon2id$", "$argon2i$v=19$m=8,t=1,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=18$m=8,t=1,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=0,t=1,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=8,t=0,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=8,t=1,p=0$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=8,t=1,p=999$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=4294967295,t=1,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5", // would allocate 4 TiB
		"$argon2id$v=19$m=8,t=4000000000,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5", // would run for hours
		"$argon2id$v=19$m=8,t=1,p=1$!!!$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=8,t=1,p=1$c2FsdHNhbHRzYWx0$!!!",
		"$argon2id$v=19$m=8,t=1,p=1$c2E$a2V5a2V5a2V5a2V5a2V5", // salt too short
		"$argon2id$v=19$m=8,t=1,p=1$c2FsdHNhbHRzYWx0$a2V5",    // key too short
		"$argon2id$v=19$m=8,x=1,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5",
		good + "$extra",
	}
	for _, h := range bad {
		ok, _, err := verifyPassword("anything at all", h, cheap)
		if ok || err == nil {
			t.Errorf("%q: ok=%v err=%v, want a refusal", h, ok, err)
		}
	}
}

func TestThePasswordPolicy(t *testing.T) {
	cases := []struct {
		pw, id, name string
		ok           bool
	}{
		{"correct horse battery", "an-id", "Thu Ngan", true},
		{"tám chữ Việt đẹp", "an-id", "Thu Ngan", true}, // length is counted in characters, not bytes
		{"short1", "an-id", "x", false},
		{"123456789", "an-id", "x", false}, // nine
		{"1234567890", "an-id", "x", false},
		{"Password123", "an-id", "x", false},
		{"aaaaaaaaaaaa", "an-id", "x", false},
		{"ababababab", "an-id", "x", false},
		{"my-thu-ngan-1-secret", "thu-ngan-1", "x", false},
		{"THU-NGAN-1-secret!", "thu-ngan-1", "x", false},
		{"nguyen van an 2024", "an-id", "Nguyen Van An", false},
		{strings.Repeat("x1y2z3", 30), "an-id", "x", false}, // 180 bytes
	}
	for _, c := range cases {
		err := CheckPassword(c.pw, c.id, c.name)
		if (err == nil) != c.ok {
			t.Errorf("CheckPassword(%q) = %v, want ok=%v", c.pw, err, c.ok)
		}
	}
}

func TestTemporaryPasswordsAreRandomAndFollowThePolicy(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		p, err := NewTemporaryPassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != 19 || p[4] != '-' || p[9] != '-' || p[14] != '-' {
			t.Fatalf("%q is not four groups of four", p)
		}
		if strings.ContainsAny(p, "0OIl1") {
			t.Fatalf("%q has a character that is mistaken for another", p)
		}
		if err := CheckPassword(p, "an-id", "Someone"); err != nil {
			t.Fatalf("%q fails the policy: %v", p, err)
		}
		if seen[p] {
			t.Fatalf("%q came up twice in 200", p)
		}
		seen[p] = true
	}
}

func TestEveryLetterOfATemporaryPasswordIsAsLikelyAsAnother(t *testing.T) {
	// 57 letters: with rejection sampling each should come up about 1/57 of the time. A plain
	// modulus of a byte would make the first 28 letters about a fifth more likely.
	count := map[byte]int{}
	total := 0
	for i := 0; i < 4000; i++ {
		p, _ := NewTemporaryPassword()
		for j := 0; j < len(p); j++ {
			if p[j] != '-' {
				count[p[j]]++
				total++
			}
		}
	}
	want := float64(total) / float64(len(tempAlphabet))
	for _, c := range []byte(tempAlphabet) {
		if got := float64(count[c]); got < want*0.8 || got > want*1.2 {
			t.Errorf("%q came up %v times, want about %v", c, got, want)
		}
	}
}
