package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/xuhuanhello/nakama-agones/internal/console"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fleet-console:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 1 && args[0] == "generate-api-token" {
		token, hash, err := console.GenerateReadAPIToken()
		if err != nil {
			return err
		}
		if json.NewEncoder(os.Stdout).Encode(map[string]string{"token": token, "token_hash": hash}) != nil {
			return errors.New("cannot write generated API token")
		}
		return nil
	}
	if len(args) > 0 && args[0] == "set-password" {
		flags := flag.NewFlagSet("set-password", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		path := flags.String("config", "", "private configuration file")
		if flags.Parse(args[1:]) != nil || *path == "" || flags.NArg() != 0 {
			return errors.New("usage: fleet-console set-password --config FILE (password on stdin)")
		}
		password, err := readPasswordStdin()
		if err != nil {
			return err
		}
		if err := console.SetPassword(*path, password); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "Password updated. Restart only fleet-console to apply it and invalidate existing sessions.")
		return nil
	}
	if len(args) == 1 && args[0] == "hash-password" {
		return hashPassword()
	}
	if len(args) == 0 || args[0] != "serve" {
		return errors.New("usage: fleet-console hash-password | generate-api-token | set-password --config FILE | serve --config FILE")
	}
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "private configuration file")
	if err := flags.Parse(args[1:]); err != nil || *configPath == "" || flags.NArg() != 0 {
		return errors.New("usage: fleet-console serve --config FILE")
	}
	cfg, err := console.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	backend, err := console.NewDataSource(cfg.Source)
	if err != nil {
		return errors.New("invalid backend source configuration")
	}
	handler, err := console.NewServer(cfg, backend, console.WebFiles())
	if err != nil {
		return err
	}
	defer handler.Close()
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return errors.New("cannot listen on the configured loopback address")
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 20 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
		ErrorLog: log.New(io.Discard, "", 0)}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("HTTP server stopped unexpectedly")
		}
		return nil
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return errors.New("HTTP server shutdown deadline exceeded")
		}
		return nil
	}
}

func readPasswordStdin() (string, error) {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice != 0 {
		return "", errors.New("supply the password through stdin, not command arguments")
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 1027))
	if err != nil || len(data) > 1026 {
		return "", errors.New("cannot read password within size limit")
	}
	password := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if strings.ContainsAny(password, "\r\n") {
		return "", errors.New("password input must contain a single line")
	}
	return password, nil
}

func hashPassword() error {
	password, err := readPasswordStdin()
	if err != nil {
		return err
	}
	encoded, err := console.HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(os.Stdout, encoded); err != nil {
		return errors.New("cannot write password hash")
	}
	return nil
}
