package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/KinasTomes/miles/internal/adminclient"
)

// command is one Admin request. Create commands name the response member
// holding the one-time secret; it goes to out and never to stdout.
type command struct {
	method, path string
	body         []byte
	out, secret  string
}

const maxRevision = 1<<53 - 1

var (
	idPattern      = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
	accessKeyID    = regexp.MustCompile(`^[A-Z0-9]{20}$`)
	tokenIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// Path-safe subset of S3 naming; the gateway applies the full rules.
	bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

func validBucket(s string) bool {
	return bucketPattern.MatchString(s) && !strings.Contains(s, "..")
}

func newFlags(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	return f
}

// parse takes flags after the positional arguments and reports which were set.
func parse(f *flag.FlagSet, args []string) (map[string]bool, bool) {
	if f.Parse(args) != nil || f.NArg() != 0 {
		return nil, false
	}
	set := map[string]bool{}
	f.Visit(func(v *flag.Flag) { set[v.Name] = true })
	return set, true
}

func listFlags(f *flag.FlagSet) (*int, *string) {
	return f.Int("limit", 100, "page limit"), f.String("cursor", "", "opaque cursor")
}

func listPath(base string, set map[string]bool, limit int, cursor string, q url.Values) (string, bool) {
	if limit < 1 || limit > 1000 || len(cursor) > 8192 {
		return "", false
	}
	if set["limit"] {
		q.Set("limit", strconv.Itoa(limit))
	}
	if set["cursor"] {
		q.Set("cursor", cursor)
	}
	if len(q) == 0 {
		return base, true
	}
	return base + "?" + q.Encode(), true
}

func jsonBody(v map[string]any) []byte {
	b, _ := json.Marshal(v) // values are strings, bools, integers and maps of them
	return b
}

func identityCommand(args []string) (command, error) {
	switch args[0] {
	case "user", "usage", "bucket", "grant", "policy", "bucket-policy", "credentials", "admin-token":
	default:
		return command{}, adminclient.ErrUnsupported
	}
	invalid := fmt.Errorf("invalid %s command; use -help", args[0])
	if len(args) < 2 {
		return command{}, invalid
	}
	switch args[0] {
	case "user":
		return userCommand(args[1], args[2:], invalid)
	case "usage":
		f := newFlags("usage")
		detail := f.Bool("detail", false, "staged bytes and active uploads")
		if len(args) < 2 || !idPattern.MatchString(args[1]) {
			return command{}, invalid
		}
		if _, ok := parse(f, args[2:]); !ok {
			return command{}, invalid
		}
		path := "/admin/v1/users/" + args[1] + "/usage"
		if *detail {
			path += "?detail=true"
		}
		return command{method: http.MethodGet, path: path}, nil
	case "bucket":
		if args[1] != "show" || len(args) != 3 || !validBucket(args[2]) {
			return command{}, invalid
		}
		return command{method: http.MethodGet, path: "/admin/v1/buckets/" + args[2]}, nil
	case "grant":
		return grantCommand(args[1], args[2:], invalid)
	case "policy":
		return policyCommand(args[1], args[2:], invalid)
	case "bucket-policy":
		return bucketPolicyCommand(args[1], args[2:], invalid)
	case "credentials":
		return credentialCommand(args[1], args[2:], invalid)
	case "admin-token":
		return tokenCommand(args[1], args[2:], invalid)
	}
	return command{}, adminclient.ErrUnsupported
}

func userCommand(sub string, args []string, invalid error) (command, error) {
	f := newFlags("user " + sub)
	switch sub {
	case "ls":
		limit, cursor := listFlags(f)
		set, ok := parse(f, args)
		if !ok {
			return command{}, invalid
		}
		path, ok := listPath("/admin/v1/users", set, *limit, *cursor, url.Values{})
		if !ok {
			return command{}, invalid
		}
		return command{method: http.MethodGet, path: path}, nil
	case "show":
		if len(args) != 1 || !idPattern.MatchString(args[0]) {
			return command{}, invalid
		}
		return command{method: http.MethodGet, path: "/admin/v1/users/" + args[0]}, nil
	case "create", "set":
	default:
		return command{}, invalid
	}
	domain := f.String("default-domain", "", "dedup domain for new buckets")
	limit := f.Uint64("logical-limit", 0, "logical quota bytes")
	status := f.String("status", "", "active|disabled")
	revision := f.Uint64("expected-revision", 0, "current user revision")
	if len(args) < 1 || !idPattern.MatchString(args[0]) {
		return command{}, invalid
	}
	set, ok := parse(f, args[1:])
	if !ok || (set["default-domain"] && !idPattern.MatchString(*domain)) || *limit > maxRevision {
		return command{}, invalid
	}
	body := map[string]any{}
	if set["default-domain"] {
		body["default_domain"] = *domain
	}
	if set["logical-limit"] {
		body["logical_limit"] = *limit
	}
	if sub == "create" {
		if set["status"] || set["expected-revision"] {
			return command{}, invalid
		}
		body["user_id"] = args[0]
		return command{method: http.MethodPost, path: "/admin/v1/users", body: jsonBody(body)}, nil
	}
	if set["status"] {
		if *status != "active" && *status != "disabled" {
			return command{}, invalid
		}
		body["status"] = *status
	}
	if len(body) == 0 || *revision < 1 || *revision > maxRevision {
		return command{}, errors.New("user set needs a field to change and --expected-revision 1..2^53-1")
	}
	body["expected_revision"] = *revision
	return command{method: http.MethodPatch, path: "/admin/v1/users/" + args[0], body: jsonBody(body)}, nil
}

func grantCommand(sub string, args []string, invalid error) (command, error) {
	f := newFlags("grant " + sub)
	if len(args) < 1 || !validBucket(args[0]) {
		return command{}, invalid
	}
	base := "/admin/v1/buckets/" + args[0] + "/grants"
	if sub == "ls" {
		limit, cursor := listFlags(f)
		set, ok := parse(f, args[1:])
		if !ok {
			return command{}, invalid
		}
		path, ok := listPath(base, set, *limit, *cursor, url.Values{})
		if !ok {
			return command{}, invalid
		}
		return command{method: http.MethodGet, path: path}, nil
	}
	if (sub != "set" && sub != "rm") || len(args) < 2 || !idPattern.MatchString(args[1]) {
		return command{}, invalid
	}
	path := base + "/" + args[1]
	bucketID := f.String("expected-bucket-id", "", "current bucket_id")
	if sub == "rm" {
		if _, ok := parse(f, args[2:]); !ok || !idPattern.MatchString(*bucketID) {
			return command{}, invalid
		}
		return command{method: http.MethodDelete, path: path + "?" + url.Values{"expected_bucket_id": {*bucketID}}.Encode()}, nil
	}
	read := f.Bool("read", false, "read right")
	write := f.Bool("write", false, "write right")
	revision := f.Uint64("expected-revision", 0, "current grant revision, 0 creates")
	set, ok := parse(f, args[2:])
	if !ok || !set["expected-revision"] || *revision > maxRevision || !idPattern.MatchString(*bucketID) {
		return command{}, errors.New("grant set needs --expected-revision 0..2^53-1 and --expected-bucket-id")
	}
	body := map[string]any{"expected_revision": *revision, "expected_bucket_id": *bucketID}
	if set["read"] {
		body["read"] = *read
	}
	if set["write"] {
		body["write"] = *write
	}
	return command{method: http.MethodPut, path: path, body: jsonBody(body)}, nil
}

func policyCommand(sub string, args []string, invalid error) (command, error) {
	if sub == "get" && len(args) == 0 {
		return command{method: http.MethodGet, path: "/admin/v1/policy"}, nil
	}
	f := newFlags("policy set")
	file := f.String("file", "", "JSON patch, at most 8 KiB")
	revision := f.Uint64("expected-revision", 0, "current policy revision")
	set, ok := parse(f, args)
	if sub != "set" || !ok || *file == "" || (set["expected-revision"] && (*revision < 1 || *revision > maxRevision)) {
		return command{}, invalid
	}
	data, err := readBounded(*file, 8<<10)
	if err != nil {
		return command{}, err
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return command{}, errors.New("policy file must be a JSON object")
	}
	if !set["expected-revision"] {
		return command{method: http.MethodPut, path: "/admin/v1/policy", body: data}, nil
	}
	if _, dup := object["expected_revision"]; dup {
		return command{}, errors.New("expected_revision given by both file and flag")
	}
	// Splice instead of re-encoding: duplicate members must reach the server,
	// which rejects them.
	start := bytes.IndexByte(data, '{') + 1
	member := `"expected_revision":` + strconv.FormatUint(*revision, 10)
	if len(object) > 0 {
		member += ","
	}
	body := append(append(append([]byte{}, data[:start]...), member...), data[start:]...)
	return command{method: http.MethodPut, path: "/admin/v1/policy", body: body}, nil
}

func bucketPolicyCommand(sub string, args []string, invalid error) (command, error) {
	if len(args) < 1 || !validBucket(args[0]) {
		return command{}, invalid
	}
	path := "/admin/v1/buckets/" + args[0] + "/policy"
	if sub == "get" && len(args) == 1 {
		return command{method: http.MethodGet, path: path}, nil
	}
	f := newFlags("bucket-policy set")
	admit := f.Bool("admit-on-write", false, "admit written chunks to cache")
	readahead := f.Uint64("readahead-chunks", 0, "readahead chunks 0..64")
	revision := f.Uint64("expected-revision", 0, "current bucket policy revision")
	bucketID := f.String("expected-bucket-id", "", "current bucket_id")
	set, ok := parse(f, args[1:])
	if sub != "set" || !ok || *readahead > 64 {
		return command{}, invalid
	}
	if *revision < 1 || *revision > maxRevision || !idPattern.MatchString(*bucketID) {
		return command{}, errors.New("bucket-policy set needs --expected-revision 1..2^53-1 and --expected-bucket-id")
	}
	cache := map[string]any{}
	if set["admit-on-write"] {
		cache["admit_on_write"] = *admit
	}
	if set["readahead-chunks"] {
		cache["readahead_chunks"] = *readahead
	}
	body := map[string]any{"cache": cache, "expected_revision": *revision, "expected_bucket_id": *bucketID}
	return command{method: http.MethodPut, path: path, body: jsonBody(body)}, nil
}

func credentialCommand(sub string, args []string, invalid error) (command, error) {
	f := newFlags("credentials " + sub)
	switch sub {
	case "rm":
		if len(args) != 1 || !accessKeyID.MatchString(args[0]) {
			return command{}, invalid
		}
		return command{method: http.MethodDelete, path: "/admin/v1/credentials/" + args[0]}, nil
	case "ls":
		return ownedList(f, "/admin/v1/credentials", args, invalid)
	case "create":
		user := f.String("user", "", "owner user_id")
		admin := f.Bool("system-admin", false, "system admin credential")
		debug := f.Bool("debug-allowed", false, "debug S3 access; needs --system-admin")
		out := f.String("out", "", "new 0600 file for the secret")
		if _, ok := parse(f, args); !ok || !idPattern.MatchString(*user) || *out == "" || (*debug && !*admin) {
			return command{}, errors.New("credentials create needs --user and --out; --debug-allowed needs --system-admin")
		}
		body := jsonBody(map[string]any{"user_id": *user, "system_admin": *admin, "debug_allowed": *debug})
		return command{method: http.MethodPost, path: "/admin/v1/credentials", body: body, out: *out, secret: "secret_access_key"}, nil
	}
	return command{}, invalid
}

func tokenCommand(sub string, args []string, invalid error) (command, error) {
	f := newFlags("admin-token " + sub)
	switch sub {
	case "revoke":
		if len(args) != 1 || !tokenIDPattern.MatchString(args[0]) {
			return command{}, invalid
		}
		return command{method: http.MethodDelete, path: "/admin/v1/admin-tokens/" + args[0]}, nil
	case "ls":
		return ownedList(f, "/admin/v1/admin-tokens", args, invalid)
	case "create":
		user := f.String("user", "", "owner user_id")
		admin := f.Bool("system-admin", false, "system admin token")
		ttl := f.Duration("ttl", 24*time.Hour, "lifetime 5m..168h")
		out := f.String("out", "", "new 0600 file for the token")
		if _, ok := parse(f, args); !ok || !idPattern.MatchString(*user) || *out == "" || !validTTL(*ttl) {
			return command{}, errors.New("admin-token create needs --user, --out and --ttl 5m..168h in whole seconds")
		}
		body := jsonBody(map[string]any{"user_id": *user, "system_admin": *admin, "expires_in_seconds": int64(*ttl / time.Second)})
		return command{method: http.MethodPost, path: "/admin/v1/admin-tokens", body: body, out: *out, secret: "token"}, nil
	}
	return command{}, invalid
}

func validTTL(d time.Duration) bool {
	return d >= 5*time.Minute && d <= 168*time.Hour && d%time.Second == 0
}

func ownedList(f *flag.FlagSet, base string, args []string, invalid error) (command, error) {
	user := f.String("user", "", "filter by user_id")
	limit, cursor := listFlags(f)
	set, ok := parse(f, args)
	if !ok || (set["user"] && !idPattern.MatchString(*user)) {
		return command{}, invalid
	}
	q := url.Values{}
	if set["user"] {
		q.Set("user_id", *user)
	}
	path, ok := listPath(base, set, *limit, *cursor, q)
	if !ok {
		return command{}, invalid
	}
	return command{method: http.MethodGet, path: path}, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read input file")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, fmt.Errorf("input file exceeds %d bytes or cannot be read", limit)
	}
	return data, nil
}

// createSecretFile refuses existing paths so a secret never overwrites another.
func createSecretFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, errors.New("cannot create secret output file (it must not exist)")
	}
	return f, nil
}

func writeSecret(f *os.File, secret string) error {
	_, err := f.WriteString(secret + "\n")
	return errors.Join(err, f.Sync(), f.Close())
}
