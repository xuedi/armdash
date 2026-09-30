package main

import (
	"bufio"
	"cmp"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"armdash/internal/auth"
)

// passwd prints the two settings that set the login in an env file. The
// normal way is the settings page, which asks for the login on first start;
// this is for installs configured by file, and for getting back in when the
// password is lost. It writes no file itself.
func passwd(args []string, in *os.File, out, msg io.Writer) error {
	fs := flag.NewFlagSet("passwd", flag.ContinueOnError)
	fs.SetOutput(msg)
	fs.Usage = func() {
		fmt.Fprintln(msg, "usage: armdash passwd [user]")
		fmt.Fprintln(msg, "Asks for a password and prints the AD_CORE_AUTH_* lines for the env file.")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return errors.New("too many arguments")
	}
	r := bufio.NewReader(in)

	user := fs.Arg(0)
	if user == "" {
		fmt.Fprint(msg, "Username [admin]: ")
		line, err := readLine(r)
		if err != nil {
			return err
		}
		user = cmp.Or(strings.TrimSpace(line), "admin")
	}
	if strings.ContainsAny(user, " \t#'\"=") {
		return fmt.Errorf("the user name %q may not contain spaces, quotes, # or =", user)
	}

	pw, err := readSecret(in, r, msg, "Password: ")
	if err != nil {
		return err
	}
	again, err := readSecret(in, r, msg, "Again: ")
	if err != nil {
		return err
	}
	if pw != again {
		return errors.New("the passwords do not match")
	}
	hash, err := auth.Hash(pw)
	if err != nil {
		return err
	}
	fmt.Fprintln(msg, "\nAdd these two lines to the env file, /etc/armdash/armdash.env for a package install, and restart armdash.")
	fmt.Fprintln(msg, "They win over the login saved on the settings page:")
	fmt.Fprintf(out, "AD_CORE_AUTH_USER=%s\nAD_CORE_AUTH_PASSWORD_HASH=%s\n", user, hash)
	return nil
}

func readLine(r *bufio.Reader) (string, error) {
	s, err := r.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || s == "") {
		return "", errors.New("no input")
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// readSecret turns echo off while the password is typed. When in is not a
// terminal it reads the line as it comes, which lets a script pipe one in.
func readSecret(in *os.File, r *bufio.Reader, msg io.Writer, prompt string) (string, error) {
	fmt.Fprint(msg, prompt)
	restore, err := echoOff(int(in.Fd()))
	if err != nil {
		return readLine(r)
	}
	// Without this, Ctrl-C at the prompt leaves the terminal not echoing.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
			restore()
			fmt.Fprintln(msg)
			os.Exit(130)
		case <-done:
		}
	}()
	defer func() {
		signal.Stop(sig)
		close(done)
		restore()
		fmt.Fprintln(msg)
	}()
	return readLine(r)
}
