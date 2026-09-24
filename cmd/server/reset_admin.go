package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"opsecmkt/internal/market"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// errNotTerminal reports that stdin is not an interactive terminal (a pipe, file or /dev/null).
var errNotTerminal = errors.New("stdin is not a terminal")

// resetAdminPassword is the -reset-admin-password break-glass: it reads the new password from stdin, connects
// with DATABASE_URL and resets that administrator's password (market.ResetAdminPassword). It starts no HTTP
// server, payment provider or background work. Success output names the handle only, never the password.
func resetAdminPassword(ctx context.Context, handle string, preview bool, args []string, in *os.File, out, prompt io.Writer) error {
	if preview {
		return errors.New("cannot be combined with -preview")
	}
	if handle == "" {
		return errors.New("give the administrator's handle: -reset-admin-password HANDLE")
	}
	if len(args) > 0 {
		return errors.New("unexpected arguments after the handle; the new password is read from stdin, never from the command line")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL required")
	}
	password, err := readNewPassword(in, prompt, handle, func() (func(), error) { return hideUntilDone(in) })
	if err != nil {
		return err
	}
	return resetWithDSN(ctx, dsn, handle, password, out)
}

// hideUntilDone turns off echo on in and also restores it if the prompt is interrupted (Ctrl-C, SIGTERM), so
// the operator's terminal is not left without echo.
func hideUntilDone(in *os.File) (func(), error) {
	restore, err := echoOff(in)
	if err != nil {
		return nil, err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		if _, ok := <-sig; ok {
			restore()
			os.Exit(130)
		}
	}()
	return func() { signal.Stop(sig); close(sig); restore() }, nil
}

// resetWithDSN opens the application database, resets the password and reports success on out.
func resetWithDSN(ctx context.Context, dsn, handle, password string, out io.Writer) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return errors.New("database configuration invalid")
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		return errors.New("database connection failed; check DATABASE_URL and service health")
	}
	if err = market.ResetAdminPassword(ctx, db, handle, password); err != nil {
		return err
	}
	fmt.Fprintf(out, "Password reset for administrator %s. Its sessions and pending sign-ins have ended; sign in with the new password.\n", handle)
	return nil
}

// readNewPassword reads the new password from in. On a terminal (hide succeeds) it turns off echo, prompts on
// prompt and asks twice; from a pipe or file it reads exactly one line (trailing newline optional). The
// password is taken byte for byte apart from the line ending; the length rule is applied by the caller.
func readNewPassword(in io.Reader, prompt io.Writer, handle string, hide func() (func(), error)) (string, error) {
	restore, err := hide()
	if errors.Is(err, errNotTerminal) {
		data, err := io.ReadAll(io.LimitReader(in, 4096))
		if err != nil {
			return "", err
		}
		line, rest, _ := strings.Cut(string(data), "\n")
		if strings.TrimRight(rest, "\r\n") != "" {
			return "", errors.New("give the new password on a single line of stdin")
		}
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			return "", errors.New("no password on stdin")
		}
		return line, nil
	}
	if err != nil {
		return "", err
	}
	defer restore()
	r := bufio.NewReader(in)
	ask := func(label string) (string, error) {
		fmt.Fprint(prompt, label)
		line, err := r.ReadString('\n')
		fmt.Fprintln(prompt) // echo is off, so the typed newline was not shown
		if err != nil {
			return "", errors.New("no password entered")
		}
		return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
	}
	first, err := ask("New password for administrator " + handle + " (12–72 bytes, not shown): ")
	if err != nil {
		return "", err
	}
	again, err := ask("Repeat the new password: ")
	if err != nil {
		return "", err
	}
	if first != again {
		return "", errors.New("the passwords do not match; nothing was changed")
	}
	return first, nil
}
