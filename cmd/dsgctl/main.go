package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
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
		fmt.Fprintln(stderr, `CLI quản trị Miles

Read: stats, pack ls
Users: user create <id> [--default-domain d] [--logical-limit n]; user ls; user show <id>
  user set <id> [--status active|disabled] [--default-domain d] [--logical-limit n] --expected-revision n
  usage <user> [--detail]
Buckets: bucket show <bucket>; grant ls <bucket>
  grant set <bucket> <user> [--read=bool] [--write=bool] --expected-revision n --expected-bucket-id id
  grant rm <bucket> <user> --expected-bucket-id id
Policy: policy get; policy set --file patch.json [--expected-revision n]
  bucket-policy get <bucket>; bucket-policy set <bucket> [--admit-on-write=bool] [--readahead-chunks n] --expected-revision n --expected-bucket-id id
Credentials: credentials create --user u [--system-admin] [--debug-allowed] --out file; credentials ls [--user u]; credentials rm <access_key_id>
Tokens: admin-token create --user u [--system-admin] [--ttl 24h] --out file; admin-token ls [--user u]; admin-token revoke <token_id>
  admin-token bootstrap --user u [--ttl 24h] --token-file file (--local-data-path dir | --ddb-table-prefix p --region r [--ddb-endpoint url])
    offline, gateway stopped; DynamoDB credentials from AWS_* env
Jobs: job ls, job show <id>, job run <type> [--dry-run] [--params-file file] [--policy-revision n], job cancel <id>
Nodes: node ls, node drain <id>
List flags: --limit 1..1000, --cursor token. Secrets go only to new 0600 --out files.
Đặt global flags trước command; command flags sau positional arguments.`)
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
	args = flags.Args()
	if len(args) >= 2 && args[0] == "admin-token" && args[1] == "bootstrap" {
		return bootstrap(ctx, args[2:], stdout)
	}
	var cmd command
	switch strings.Join(args, " ") {
	case "stats":
		cmd = command{method: http.MethodGet, path: "/admin/v1/stats"}
	case "pack ls":
		cmd = command{method: http.MethodGet, path: "/admin/v1/packs"}
	default:
		var err error
		if args[0] == "job" || args[0] == "node" {
			cmd.method, cmd.path, cmd.body, err = operationCommand(args)
		} else {
			cmd, err = identityCommand(args)
		}
		if err != nil {
			return err
		}
	}
	if *tokenFile == "" {
		return errors.New("--token-file is required")
	}
	client, err := adminclient.New(*endpoint, *tokenFile, *timeout)
	if err != nil {
		return err
	}
	defer client.Close()
	var out *os.File
	if cmd.out != "" {
		if out, err = createSecretFile(cmd.out); err != nil {
			return err
		}
	}
	b, err := client.Request(ctx, cmd.method, cmd.path, cmd.body)
	if out != nil {
		b, err = storeSecret(out, cmd.secret, b, err)
	}
	if err != nil || b == nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(b))
	return err
}

// storeSecret moves the one-time secret from the response into out. Without a
// secret in a successful response the empty file is removed.
func storeSecret(out *os.File, member string, b []byte, err error) ([]byte, error) {
	var secret string
	if err == nil {
		var object map[string]json.RawMessage
		if json.Unmarshal(b, &object) != nil || json.Unmarshal(object[member], &secret) != nil || secret == "" {
			err = errors.New("Admin response has no " + member)
		}
	}
	if err != nil {
		_ = out.Close()
		_ = os.Remove(out.Name())
		return nil, err
	}
	if writeSecret(out, secret) != nil {
		// The resource exists; print it (redacted) so the operator can revoke it.
		return nil, fmt.Errorf("cannot persist secret to %s; revoke this resource: %s", out.Name(), bytes.ReplaceAll(b, []byte(secret), []byte("[REDACTED]")))
	}
	return bytes.ReplaceAll(b, []byte(secret), []byte("[REDACTED]")), nil
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
