package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/KinasTomes/miles/internal/app"
	"github.com/KinasTomes/miles/internal/buildinfo"
	"github.com/KinasTomes/miles/internal/config"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("miles", flag.ContinueOnError)
	flags.SetOutput(stderr)
	version := flags.Bool("version", false, "Hiển thị phiên bản")
	file := flags.String("config", "", "Startup JSON config (default runtime is unready until adapters are wired)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *version {
		fmt.Fprintln(stdout, buildinfo.String("miles"))
		return nil
	}
	c := config.DefaultStartup()
	if *file != "" {
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		c, err = config.LoadStartup(f)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	d, err := app.Bootstrap(c)
	if err != nil {
		return err
	}
	return app.Run(ctx, c, d, stderr)
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "miles:", err)
		os.Exit(1)
	}
}
