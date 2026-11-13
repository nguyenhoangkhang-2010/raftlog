package accounts

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// HashParams are the costs of argon2id. Memory is in KiB.
type HashParams struct {
	Memory      uint32
	Iterations  uint32
	Parallelism uint8
}

// DefaultHash is 64 MiB, 3 passes and 2 lanes: roughly a tenth of a second on a current laptop,
// above the minimum that OWASP recommends for argon2id. It is a cost the service pays at every
// sign-in, so the service limits how many run at once (see Config.MaxHashes).
var DefaultHash = HashParams{Memory: 64 * 1024, Iterations: 3, Parallelism: 2}

const (
	saltLength = 16
	keyLength  = 32
)

// hashPassword returns the PHC string of a password: the algorithm, its parameters, a fresh
// random salt and the key, so a stored hash says how to check itself.
func hashPassword(password string, p HashParams) (string, error) {
	salt := make([]byte, saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("accounts: no randomness for a salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, keyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.Memory, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

var errBadHash = errors.New("accounts: the stored password hash is not an argon2id hash this program understands")

// maxStoredMemory bounds what a stored hash may ask of the verifier: a row that was tampered
// with must not make a sign-in allocate gigabytes.
const maxStoredMemory = 1 << 20 // 1 GiB in KiB; far above any real setting

// verifyPassword reports whether the password matches the PHC string, and whether the hash was
// made with weaker parameters than p, so that the caller can store a fresh one.
func verifyPassword(password, encoded string, p HashParams) (ok, rehash bool, err error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, key
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, false, errBadHash
	}
	if v, ok := strings.CutPrefix(parts[2], "v="); !ok || v != strconv.Itoa(argon2.Version) {
		return false, false, errBadHash
	}
	var m, t uint64
	var par uint64
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, found := strings.Cut(kv, "=")
		if !found {
			return false, false, errBadHash
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return false, false, errBadHash
		}
		switch k {
		case "m":
			m = n
		case "t":
			t = n
		case "p":
			par = n
		default:
			return false, false, errBadHash
		}
	}
	if m == 0 || t == 0 || par == 0 || par > 255 || m > maxStoredMemory || t > 100 {
		return false, false, errBadHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return false, false, errBadHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) < 16 {
		return false, false, errBadHash
	}
	got := argon2.IDKey([]byte(password), salt, uint32(t), uint32(m), uint8(par), uint32(len(want)))
	ok = subtle.ConstantTimeCompare(got, want) == 1
	rehash = ok && (uint32(m) < p.Memory || uint32(t) < p.Iterations || uint8(par) != p.Parallelism)
	return ok, rehash, nil
}

// --- the policy --------------------------------------------------------------------------

// Password length limits. The upper bound is not a rule about security but about cost: a
// request must not be able to make the service hash a megabyte.
const (
	MinPasswordLength = 10
	MaxPasswordBytes  = 128
)

// PasswordError says why a password was refused.
type PasswordError struct{ Reason string }

func (e *PasswordError) Error() string { return "accounts: " + e.Reason }

// commonPasswords are passwords of ten characters or more that are in every list of the most
// used. It is not a check against known breaches (that needs a service or a large file); it
// stops the few choices that would be tried first.
var commonPasswords = map[string]bool{
	"1234567890": true, "0123456789": true, "0987654321": true, "1q2w3e4r5t": true, "qwertyuiop": true,
	"qwerty1234": true, "qwerty12345": true, "password12": true, "password123": true, "password1234": true,
	"abcdefghij": true, "abcd123456": true, "iloveyou12": true, "iloveyou123": true, "administrator": true,
	"welcome123": true, "welcome1234": true, "letmein123": true, "changeme123": true, "changemenow": true,
	"1111111111": true, "0000000000": true, "123456789a": true, "a123456789": true, "asdfghjkl1": true,
}

// CheckPassword applies the policy: ten characters or more, not a name of the account, not one of
// the most common, and not made of one or two characters repeated.
func CheckPassword(password, userID, displayName string) error {
	switch {
	case utf8.RuneCountInString(password) < MinPasswordLength:
		return &PasswordError{fmt.Sprintf("a password is at least %d characters", MinPasswordLength)}
	case len(password) > MaxPasswordBytes:
		return &PasswordError{fmt.Sprintf("a password is at most %d bytes", MaxPasswordBytes)}
	}
	lower := strings.ToLower(password)
	if commonPasswords[lower] {
		return &PasswordError{"that password is one of the most used; choose another"}
	}
	if userID != "" && strings.Contains(lower, strings.ToLower(userID)) {
		return &PasswordError{"a password cannot contain the user's login name"}
	}
	if displayName != "" && len(displayName) >= 4 && strings.Contains(lower, strings.ToLower(displayName)) {
		return &PasswordError{"a password cannot contain the user's name"}
	}
	distinct := map[rune]bool{}
	for _, r := range password {
		distinct[r] = true
	}
	if len(distinct) < 5 {
		return &PasswordError{"a password needs at least 5 different characters"}
	}
	return nil
}

// tempAlphabet leaves out the characters that are mistaken for each other when read out or
// copied by hand: 0 and O, 1 and l and I.
const tempAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// NewTemporaryPassword returns a random password of 16 characters in four groups, such as
// "k7Tq-mP3x-Wd9R-hv4N": about 93 bits, shown once to the person who asked for it.
func NewTemporaryPassword() (string, error) {
	const groups, per = 4, 4
	out := make([]byte, 0, groups*per+groups-1)
	limit := byte(256 - 256%len(tempAlphabet)) // reject from here up so that every letter is as likely
	buf := make([]byte, 1)
	for g := 0; g < groups; g++ {
		if g > 0 {
			out = append(out, '-')
		}
		for i := 0; i < per; {
			if _, err := rand.Read(buf); err != nil {
				return "", fmt.Errorf("accounts: no randomness: %w", err)
			}
			if buf[0] >= limit {
				continue
			}
			out = append(out, tempAlphabet[int(buf[0])%len(tempAlphabet)])
			i++
		}
	}
	return string(out), nil
}
