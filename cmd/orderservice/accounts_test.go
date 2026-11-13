package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgtest"
)

func TestSettingsOfTheAccounts(t *testing.T) {
	menu := filepath.Join(sample, "menu.json")
	base := []string{"-addr", "a:1", "-menu", menu}
	url := "postgres://u:p@127.0.0.1:5432/db"

	cfg, err := parseConfig(base, env(map[string]string{envDatabase: url}), io.Discard)
	if err != nil || !cfg.withAccounts() || cfg.sessionIdle != 12*time.Hour || cfg.sessionMax != 7*24*time.Hour || cfg.secureCookies || cfg.noMigrate {
		t.Fatalf("defaults: %v %+v", err, cfg)
	}
	cfg, err = parseConfig(append([]string{"-session-idle", "2h", "-session-max", "24h", "-secure-cookies", "-no-migrate"}, base...), env(map[string]string{envDatabase: url}), io.Discard)
	if err != nil || cfg.sessionIdle != 2*time.Hour || cfg.sessionMax != 24*time.Hour || !cfg.secureCookies || !cfg.noMigrate {
		t.Fatalf("flags: %v %+v", err, cfg)
	}
	cfg, err = parseConfig(base, env(map[string]string{envDatabase: url, envSessionIdle: "30m", envSessionMax: "8h", envSecureCookies: "true"}), io.Discard)
	if err != nil || cfg.sessionIdle != 30*time.Minute || cfg.sessionMax != 8*time.Hour || !cfg.secureCookies {
		t.Fatalf("variables: %v %+v", err, cfg)
	}
	users := filepath.Join(sample, "users.json")
	for name, c := range map[string]struct {
		args []string
		vars map[string]string
	}{
		"both ways to sign in":         {append([]string{"-users", users}, base...), map[string]string{envDatabase: url}},
		"neither way to sign in":       {base, nil},
		"idle longer than the max":     {base, map[string]string{envDatabase: url, envSessionIdle: "48h", envSessionMax: "24h"}},
		"a session of no length":       {append([]string{"-session-max", "0s"}, base...), map[string]string{envDatabase: url}},
		"a bad duration in a variable": {base, map[string]string{envDatabase: url, envSessionIdle: "a while"}},
	} {
		if _, err := parseConfig(c.args, env(c.vars), io.Discard); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTheDatabasePasswordIsNeverInAnError(t *testing.T) {
	const secret = "s3cr3t-pa55word"
	for _, url := range []string{
		"postgres://kitchen:" + secret + "@127.0.0.1:1/kitchen?connect_timeout=1", // nothing listens
		"postgres://kitchen:" + secret + "@127.0.0.1:1/kitchen?sslmode=sometimes", // not a valid setting
		"postgres://kitchen:" + secret + "@[::1/kitchen",                          // not a URL
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _, err := openAccounts(ctx, config{databaseURL: url}, nil)
		cancel()
		if err == nil {
			t.Fatalf("%q: no error", url)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error shows the password: %v", err)
		}
	}
}

// asker answers the password questions of a command, and remembers them.
func asker(answers ...string) (passwordFunc, *[]string) {
	var prompts []string
	return func(prompt string, _ bool) (string, error) {
		prompts = append(prompts, prompt)
		if len(answers) == 0 {
			return "", errors.New("no more answers")
		}
		a := answers[0]
		answers = answers[1:]
		return a, nil
	}, &prompts
}

func runCmd(t *testing.T, url string, ask passwordFunc, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runUser(context.Background(), args, env(map[string]string{envDatabase: url}), ask, &out, io.Discard)
	return out.String(), err
}

func TestTheFirstAdministratorIsMadeAtTheCommandLine(t *testing.T) {
	url := pgtest.URL(t)
	ask, prompts := asker("a long enough password")
	out, err := runCmd(t, url, ask, "create", "-id", "Admin-1", "-name", "The Administrator", "-roles", "admin")
	if err != nil || !strings.Contains(out, "created admin-1") {
		t.Fatalf("create: %q %v", out, err)
	}
	if len(*prompts) != 1 || !strings.Contains((*prompts)[0], "admin-1") {
		t.Fatalf("prompts %v", *prompts)
	}
	out, err = runCmd(t, url, nil, "list")
	if err != nil || !strings.Contains(out, "admin-1") || !strings.Contains(out, "The Administrator") || !strings.Contains(out, "active") || strings.Contains(out, "password") {
		t.Fatalf("list: %q %v", out, err)
	}
}

func TestTheCommandLineRefusesWhatTheServiceWouldRefuse(t *testing.T) {
	url := pgtest.URL(t)
	good := "a long enough password"
	for name, c := range map[string]struct {
		pw   string
		args []string
		want string
	}{
		"a weak password":                  {"short", []string{"create", "-id", "adm-1", "-name", "A", "-roles", "admin"}, "at least"},
		"an unknown role":                  {good, []string{"create", "-id", "adm-1", "-name", "A", "-roles", "owner"}, "not a role"},
		"a kitchen worker with no station": {good, []string{"create", "-id", "adm-1", "-name", "A", "-roles", "kitchen"}, "station"},
		"a missing flag":                   {good, []string{"create", "-id", "adm-1", "-roles", "admin"}, "-name"},
		"an unknown command":               {good, []string{"destroy"}, "unknown command"},
		"no command":                       {good, nil, "give a command"},
		"a stray argument":                 {good, []string{"list", "stray"}, "unexpected"},
		"set-password of nobody":           {good, []string{"set-password", "-id", "nobody-here"}, "not found"},
	} {
		ask, _ := asker(c.pw)
		_, err := runCmd(t, url, ask, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error with %q", name, err, c.want)
		}
	}
	if err := runUser(context.Background(), []string{"list"}, env(nil), nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "database") {
		t.Errorf("no database address: %v", err)
	}
}

func TestARecoveredAccountCanBeCreatedTwiceNeverButItsPasswordCanBeSet(t *testing.T) {
	url := pgtest.URL(t)
	ask, _ := asker("first long password", "first long password", "second long password")
	if _, err := runCmd(t, url, ask, "create", "-id", "adm-1", "-name", "A", "-roles", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, url, ask, "create", "-id", "adm-1", "-name", "A", "-roles", "admin"); err == nil || !strings.Contains(err.Error(), "taken") {
		t.Fatalf("a login name created twice: %v", err)
	}
	if out, err := runCmd(t, url, ask, "set-password", "-id", "adm-1"); err != nil || !strings.Contains(out, "sessions have ended") {
		t.Fatalf("set-password: %q %v", out, err)
	}
}

func TestAPasswordCanComeFromAPipe(t *testing.T) {
	for in, want := range map[string]string{
		"from a pipe\n":    "from a pipe",
		"no newline":       "no newline",
		"windows line\r\n": "windows line",
		"first\nsecond\n":  "first",
		" spaces kept \n":  " spaces kept ",
	} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		go func() { _, _ = w.WriteString(in); w.Close() }()
		got, err := terminalPassword(r, io.Discard)("Password: ", true)
		r.Close()
		if err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", in, got, err, want)
		}
	}
	r, w, _ := os.Pipe()
	w.Close()
	if _, err := terminalPassword(r, io.Discard)("Password: ", false); err == nil {
		t.Error("an empty input gave a password")
	}
	r.Close()
}
