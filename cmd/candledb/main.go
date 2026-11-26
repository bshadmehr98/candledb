// Command candledb is the database server.
//
// This package is the composition root of the hexagon: the one place that
// knows every concrete type. It builds the adapters, hands them to the
// application core through the ports, and starts the HTTP server. Swapping
// SQLite for another store, or HTTP for another transport, means changing
// lines here and nowhere in the core.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// version is replaced at build time: go build -ldflags "-X main.version=1.2.3"
var version = "dev"

const usage = `candledb - a lightweight document database with realtime sync

Usage:
  candledb serve [flags]          run the server
  candledb check-rules <file>     validate a security rules file
  candledb backup [flags] <dest>  write a consistent copy of the database
  candledb version                print the version

Run "candledb <command> -h" to see a command's flags.
`

func main() {
	// NotifyContext returns a context that is cancelled when the process
	// receives one of the signals: the idiomatic way to turn Ctrl-C and
	// "docker stop" into an orderly shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "candledb:", err)
		}
		os.Exit(1)
	}
}

// run is main without the parts that make main hard to test: it takes its
// arguments and output streams as parameters and returns an error instead
// of exiting.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return flag.ErrHelp
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:], stderr)
	case "check-rules":
		return checkRules(args[1:], stdout)
	case "backup":
		return backup(ctx, args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	}
	fmt.Fprint(stderr, usage)
	return fmt.Errorf("unknown command %q", args[0])
}
