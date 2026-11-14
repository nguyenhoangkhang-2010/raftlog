package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/accounts"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
)

const userUsage = `Usage:
  orderservice user create       -id NAME -name "Full Name" -roles admin[,role...] [-station drinks]
  orderservice user set-password -id NAME
  orderservice user list

These work on the accounts database directly, so they are how the first administrator is made
and how an account is recovered when every administrator is locked out. The database comes from
-database-url or ORDERSERVICE_DATABASE_URL. The password is asked for twice at the terminal
(nothing is shown as you type), or read from the first line of the input when it is piped.
`

// passwordFunc asks for a password; prompt is shown to a person at a terminal.
type passwordFunc func(prompt string, confirm bool) (string, error)

// terminalPassword reads a password from the terminal without echoing it, asking twice when it is
// new; from a pipe, it reads one line.
func terminalPassword(in *os.File, out io.Writer) passwordFunc {
	return func(prompt string, confirm bool) (string, error) {
		if !term.IsTerminal(int(in.Fd())) {
			line, err := bufio.NewReader(in).ReadString('\n')
			if err != nil && (err != io.EOF || line == "") {
				return "", errors.New("no password on the input")
			}
			return strings.TrimRight(line, "\r\n"), nil
		}
		fmt.Fprint(out, prompt)
		pw, err := term.ReadPassword(int(in.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return "", err
		}
		if confirm {
			fmt.Fprint(out, "Again: ")
			again, err := term.ReadPassword(int(in.Fd()))
			fmt.Fprintln(out)
			if err != nil {
				return "", err
			}
			if string(again) != string(pw) {
				return "", errors.New("the two passwords differ")
			}
		}
		return string(pw), nil
	}
}

// runUser runs a subcommand of "orderservice user".
func runUser(ctx context.Context, args []string, getenv func(string) string, ask passwordFunc, out, errOut io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(errOut, userUsage)
		return errors.New("give a command: create, set-password or list")
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet("orderservice user "+cmd, flag.ContinueOnError)
	fs.SetOutput(errOut)
	cfg := config{databaseURL: getenv(envDatabase), sessionIdle: defaultSessionIdle, sessionMax: defaultSessionMax}
	fs.StringVar(&cfg.databaseURL, "database-url", cfg.databaseURL, "PostgreSQL that holds the accounts ["+envDatabase+"]")
	fs.BoolVar(&cfg.noMigrate, "no-migrate", false, "do not bring the database schema up to date first")
	var id, name, roles, station string
	switch cmd {
	case "create":
		fs.StringVar(&id, "id", "", "the login name: 3 to 32 lower-case letters, digits, dots, dashes, underscores")
		fs.StringVar(&name, "name", "", "the name shown on screens")
		fs.StringVar(&roles, "roles", "", "roles, comma separated: customer, cashier, kitchen, runner, manager, storekeeper, admin")
		fs.StringVar(&station, "station", "", "the station of a kitchen worker, such as drinks")
	case "set-password":
		fs.StringVar(&id, "id", "", "the login name")
	case "list":
	default:
		fmt.Fprint(errOut, userUsage)
		return fmt.Errorf("unknown command %q", cmd)
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if cfg.databaseURL == "" {
		return errors.New("no database: give -database-url or set " + envDatabase)
	}

	logger := slog.New(slog.NewTextHandler(errOut, &slog.HandlerOptions{Level: slog.LevelWarn}))
	pool, acc, err := openAccounts(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer pool.Close()

	switch cmd {
	case "create":
		if id == "" || name == "" || roles == "" {
			return errors.New("give -id, -name and -roles")
		}
		var rs []rbac.Role
		for _, r := range strings.Split(roles, ",") {
			rs = append(rs, rbac.Role(strings.TrimSpace(r)))
		}
		pw, err := ask("Password for "+strings.ToLower(strings.TrimSpace(id))+": ", true)
		if err != nil {
			return err
		}
		u, err := acc.CreateWithPassword(ctx, "command-line", accounts.NewUser{ID: id, DisplayName: name, Roles: rs, Station: station}, pw)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "created %s (%s) with roles %s\n", u.ID, u.DisplayName, roles)
	case "set-password":
		if id == "" {
			return errors.New("give -id")
		}
		pw, err := ask("New password for "+strings.ToLower(strings.TrimSpace(id))+": ", true)
		if err != nil {
			return err
		}
		if err := acc.SetPassword(ctx, id, pw); err != nil {
			return err
		}
		fmt.Fprintf(out, "the password of %s is set, its lock is cleared and its sessions have ended\n", id)
	case "list":
		users, err := acc.ListUsers(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tROLES\tSTATION\tSTATE")
		for _, u := range users {
			state := "active"
			switch {
			case u.Disabled:
				state = "disabled"
			case u.MustChangePassword:
				state = "must change password"
			}
			rs := make([]string, len(u.Roles))
			for i, r := range u.Roles {
				rs[i] = string(r)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", u.ID, u.DisplayName, strings.Join(rs, ","), u.Station, state)
		}
		return tw.Flush()
	}
	return nil
}
