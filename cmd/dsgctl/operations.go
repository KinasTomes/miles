package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"unicode/utf8"

	"github.com/KinasTomes/miles/internal/adminclient"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/storage"
)

// packPart is a path-safe pack_id or generation; the gateway checks existence.
var packPart = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}$`)

// operationCommand builds the system-admin statistics, pack, cache, job and
// node commands. Scope errors stay local: no request leaves for them.
func operationCommand(args []string) (command, error) {
	invalid := fmt.Errorf("invalid %s command; use -help", args[0])
	switch args[0] {
	case "stats":
		f := newFlags("stats")
		bucket := f.String("bucket", "", "bucket name")
		set, ok := parse(f, args[1:])
		if !ok || (set["bucket"] && !validBucket(*bucket)) {
			return command{}, invalid
		}
		if set["bucket"] {
			return command{method: http.MethodGet, path: "/admin/v1/buckets/" + *bucket + "/stats"}, nil
		}
		return command{method: http.MethodGet, path: "/admin/v1/stats"}, nil
	case "layout":
		// No flags: a key may start with "-".
		if len(args) != 3 || !validBucket(args[1]) || args[2] == "" || len(args[2]) > 1024 || !utf8.ValidString(args[2]) {
			return command{}, invalid
		}
		q := url.Values{"bucket": {args[1]}, "key": {args[2]}}
		return command{method: http.MethodGet, path: "/admin/v1/objects/layout?" + q.Encode()}, nil
	}
	if len(args) < 2 {
		return command{}, invalid
	}
	switch args[0] {
	case "pack":
		return packCommand(args[1], args[2:], invalid)
	case "fault":
		return faultCommand(args[1], args[2:], invalid)
	case "cache":
		return cacheCommand(args[1], args[2:], invalid)
	case "job", "node":
		return jobCommand(args[0], args[1], args[2:], invalid)
	}
	return command{}, adminclient.ErrUnsupported
}

func faultCommand(sub string, args []string, invalid error) (command, error) {
	f := newFlags("fault " + sub)
	switch sub {
	case "ls":
		if len(args) != 0 {
			return command{}, invalid
		}
		return command{method: http.MethodGet, path: "/admin/v1/faults"}, nil
	case "rm":
		if len(args) != 1 || args[0] == "" {
			return command{}, errors.New("fault rm needs <fault_id>")
		}
		return command{method: http.MethodDelete, path: "/admin/v1/faults/" + args[0]}, nil
	case "add":
		faultType := f.String("type", "", "fault type: s3_5xx|ddb_throttle|kill_node|corrupt_pack")
		scope := f.String("scope", "node", "scope: node (cluster scope deferred to multi-node task)")
		nodeID := f.String("node", "", "node id for --scope node")
		rate := f.Float64("rate", 1.0, "injection probability (0.0, 1.0]")
		duration := f.Int64("duration", 60, "duration in seconds (1..86400)")
		set, ok := parse(f, args)
		if !ok || !set["type"] || *rate <= 0.0 || *rate > 1.0 || *duration <= 0 || *scope != "node" || !set["node"] {
			return command{}, invalid
		}
		body := map[string]any{
			"type":             *faultType,
			"scope":            *scope,
			"node_id":          *nodeID,
			"rate":             *rate,
			"duration_seconds": *duration,
		}
		return command{method: http.MethodPost, path: "/admin/v1/faults", body: jsonBody(body)}, nil
	default:
		return command{}, invalid
	}
}

func packCommand(sub string, args []string, invalid error) (command, error) {
	f := newFlags("pack " + sub)
	switch sub {
	case "ls":
		filters := map[string]*string{
			"state":         f.String("state", "", "pack state"),
			"storage_class": f.String("storage-class", "", "storage class"),
			"live_ratio_lt": f.String("live-ratio-lt", "", "decimal live ratio 0..1"),
		}
		limit, cursor := listFlags(f)
		set, ok := parse(f, args)
		if !ok {
			return command{}, invalid
		}
		// The gateway validates filter values; Encode keeps them inside the query.
		q := url.Values{}
		for name, v := range filters {
			if *v != "" {
				q.Set(name, *v)
			}
		}
		path, ok := listPath("/admin/v1/packs", set, *limit, *cursor, q)
		if !ok {
			return command{}, invalid
		}
		return command{method: http.MethodGet, path: path}, nil
	case "show":
		if len(args) < 1 || !packPart.MatchString(args[0]) {
			return command{}, invalid
		}
		limit, cursor := listFlags(f)
		set, ok := parse(f, args[1:])
		if !ok {
			return command{}, invalid
		}
		path, ok := listPath("/admin/v1/packs/"+args[0], set, *limit, *cursor, url.Values{})
		if !ok {
			return command{}, invalid
		}
		return command{method: http.MethodGet, path: path}, nil
	case "restore", "promote":
	default:
		return command{}, invalid
	}
	if len(args) < 2 || !packPart.MatchString(args[0]) || !packPart.MatchString(args[1]) {
		return command{}, errors.New("pack " + sub + " needs <pack_id> <generation>")
	}
	path := "/admin/v1/packs/" + args[0] + "/" + args[1] + "/" + sub
	if sub == "promote" {
		if len(args) != 2 {
			return command{}, invalid
		}
		return command{method: http.MethodPost, path: path, body: []byte(`{}`)}, nil
	}
	days := f.Int("days", 1, "restore days 1..30")
	tier := f.String("tier", "Standard", "Standard|Bulk|Expedited")
	set, ok := parse(f, args[2:])
	if !ok || *days < 1 || *days > 30 || (*tier != "Standard" && *tier != "Bulk" && *tier != "Expedited") {
		return command{}, errors.New("pack restore needs --days 1..30 and --tier Standard|Bulk|Expedited")
	}
	body := map[string]any{}
	if set["days"] {
		body["days"] = *days
	}
	if set["tier"] {
		body["tier"] = *tier
	}
	return command{method: http.MethodPost, path: path, body: jsonBody(body)}, nil
}

func cacheCommand(sub string, args []string, invalid error) (command, error) {
	if sub != "stats" && sub != "purge" {
		return command{}, invalid
	}
	f := newFlags("cache " + sub)
	scope := f.String("scope", "", "node|cluster")
	node := f.String("node", "", "node id for --scope node")
	set, ok := parse(f, args)
	if !ok || !(*scope == "cluster" && !set["node"] || *scope == "node" && jobs.ValidID(*node)) {
		return command{}, errors.New("cache " + sub + " needs --scope cluster, or --scope node --node <id>")
	}
	if sub == "purge" {
		body := map[string]any{"scope": *scope}
		if set["node"] {
			body["node_id"] = *node
		}
		return command{method: http.MethodPost, path: "/admin/v1/cache/purge", body: jsonBody(body)}, nil
	}
	q := url.Values{"scope": {*scope}}
	if set["node"] {
		q.Set("node_id", *node)
	}
	return command{method: http.MethodGet, path: "/admin/v1/cache/stats?" + q.Encode()}, nil
}

func jobCommand(kind, sub string, args []string, invalid error) (command, error) {
	base := "/admin/v1/jobs"
	if kind == "node" {
		base = "/admin/v1/cluster/nodes"
	}
	f := newFlags(kind + " " + sub)
	if sub == "ls" {
		limit, cursor := listFlags(f)
		set, ok := parse(f, args)
		if !ok {
			return command{}, invalid
		}
		path, ok := listPath(base, set, *limit, *cursor, url.Values{})
		if !ok {
			return command{}, invalid
		}
		return command{method: http.MethodGet, path: path}, nil
	}
	if kind == "job" && sub == "run" {
		if len(args) < 1 || !jobs.Type(args[0]).Valid() {
			return command{}, fmt.Errorf("invalid job type; supported types: %v", jobs.Types())
		}
		dry := f.Bool("dry-run", false, "dry run")
		revision := f.Uint64("policy-revision", 0, "pinned policy revision")
		params := f.String("params-file", "", "JSON params")
		if _, ok := parse(f, args[1:]); !ok || *revision > storage.MaxPolicyRevision {
			return command{}, invalid
		}
		doc := json.RawMessage(`{}`)
		if *params != "" {
			data, err := readBounded(*params, 8192)
			if err != nil {
				return command{}, err
			}
			var object map[string]json.RawMessage
			if json.Unmarshal(data, &object) != nil || object == nil {
				return command{}, errors.New("params must be a JSON object")
			}
			doc = data
		}
		body, err := json.Marshal(jobs.Create{Type: jobs.Type(args[0]), DryRun: *dry, Params: doc, PolicyRevision: *revision})
		return command{method: http.MethodPost, path: base, body: body}, err
	}
	if len(args) != 1 || !jobs.ValidID(args[0]) {
		return command{}, invalid
	}
	switch {
	case kind == "job" && sub == "show":
		return command{method: http.MethodGet, path: base + "/" + args[0]}, nil
	case kind == "job" && sub == "cancel":
		return command{method: http.MethodDelete, path: base + "/" + args[0]}, nil
	case kind == "node" && sub == "drain":
		return command{method: http.MethodPost, path: base + "/" + args[0] + "/drain", body: []byte(`{}`)}, nil
	}
	return command{}, invalid
}
