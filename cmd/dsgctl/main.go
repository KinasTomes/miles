package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/KinasTomes/miles/internal/adminclient"
	"github.com/KinasTomes/miles/internal/buildinfo"
)

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("dsgctl", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	version := flags.Bool("version", false, "Hiển thị phiên bản")
	endpoint := flags.String("endpoint", "http://127.0.0.1:9000", "Admin API origin; HTTP chỉ qua loopback/tunnel, remote dùng HTTPS")
	tokenFile := flags.String("token-file", "", "Bắt buộc: bearer token file permission 0600; không truyền raw token")
	timeout := flags.Duration("timeout", 30*time.Second, "Thời hạn toàn bộ request, tối đa 30m")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "CLI quản trị Miles\n\nRead commands: stats, policy get, user ls, pack ls, job ls, node ls\nCác lệnh còn lại chưa hỗ trợ. Đặt flags trước command.")
		flags.SetOutput(stderr)
		flags.PrintDefaults()
		flags.SetOutput(io.Discard)
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errors.New("invalid CLI flags; use -help")
	}
	if *version {
		fmt.Fprintln(stdout, buildinfo.String("dsgctl"))
		return nil
	}
	if flags.NArg() == 0 {
		flags.Usage()
		return errors.New("a command is required")
	}
	path := ""
	switch strings.Join(flags.Args(), " ") {
	case "stats":
		path = "/admin/v1/stats"
	case "policy get":
		path = "/admin/v1/policy"
	case "user ls":
		path = "/admin/v1/users"
	case "pack ls":
		path = "/admin/v1/packs"
	case "job ls":
		path = "/admin/v1/jobs"
	case "node ls":
		path = "/admin/v1/cluster/nodes"
	default:
		return adminclient.ErrUnsupported
	}
	if *tokenFile == "" {
		return errors.New("--token-file is required")
	}
	client, err := adminclient.New(*endpoint, *tokenFile, *timeout)
	if err != nil {
		return err
	}
	defer client.Close()
	b, err := client.Get(ctx, path)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(b))
	return err
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "dsgctl:", err)
		os.Exit(1)
	}
}
