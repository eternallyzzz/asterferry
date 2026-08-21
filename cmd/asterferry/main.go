package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"asterferry/internal/agent"
	"asterferry/internal/config"
	"asterferry/internal/gateway"
	"asterferry/internal/logging"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: asterferry <gateway|agent|validate> -c config.yaml")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	path := fs.String("c", "config.yaml", "configuration file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	c, err := config.Load(*path)
	if err != nil {
		return err
	}
	if err := config.ApplyEnv(c); err != nil {
		return err
	}
	switch args[0] {
	case "validate":
		fmt.Printf("valid %s configuration\n", c.Role)
		return nil
	case "gateway":
		if c.Role != config.RoleGateway {
			return errors.New("configuration mode is not gateway")
		}
		opts, err := c.ResolveGateway()
		if err != nil {
			return err
		}
		logger, closeLog, err := logging.New(opts.Logging, c.Role, os.Stderr)
		if err != nil {
			return err
		}
		defer closeLog()
		s, err := gateway.New(opts, logger)
		if err != nil {
			return err
		}
		if err = s.Start(); err != nil {
			return err
		}
		return wait(s.Close)
	case "agent":
		if c.Role != config.RoleAgent {
			return errors.New("configuration mode is not agent")
		}
		opts, err := c.ResolveAgent()
		if err != nil {
			return err
		}
		logger, closeLog, err := logging.New(opts.Logging, c.Role, os.Stderr)
		if err != nil {
			return err
		}
		defer closeLog()
		a, err := agent.New(opts, logger)
		if err != nil {
			return err
		}
		if err = a.Start(); err != nil {
			return err
		}
		return wait(a.Close)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func wait(closeFn func() error) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	return closeFn()
}
