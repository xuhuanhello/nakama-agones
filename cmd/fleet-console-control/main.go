package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/xuhuanhello/nakama-agones/internal/console"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fleet-console-control:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("fleet-console-control", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "", "root-owned private configuration")
	if flags.Parse(args) != nil || *path == "" || flags.NArg() != 0 {
		return errors.New("usage: fleet-console-control --config FILE")
	}
	if os.Geteuid() != 0 {
		return errors.New("control broker must run as root")
	}
	cfg, err := console.LoadControlConfig(*path)
	if err != nil {
		return err
	}
	handler, err := console.NewControlServer(cfg)
	if err != nil {
		return err
	}
	listener, err := console.ListenControlSocket(cfg)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 6 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 8 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("control server stopped unexpectedly")
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		if server.Shutdown(shutdown) != nil {
			server.Close()
			return errors.New("control shutdown deadline exceeded")
		}
		return nil
	}
}
