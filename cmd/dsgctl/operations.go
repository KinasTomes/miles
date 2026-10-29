package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"github.com/KinasTomes/miles/internal/adminclient"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/storage"
)

func operationCommand(args []string) (method, path string, body []byte, err error) {
	if len(args) < 2 || (args[0] != "job" && args[0] != "node") {
		return "", "", nil, adminclient.ErrUnsupported
	}
	invalid := errors.New("invalid job/node command; use -help")
	base := "/admin/v1/jobs"
	if args[0] == "node" {
		base = "/admin/v1/cluster/nodes"
	}
	if args[1] == "ls" {
		f := flag.NewFlagSet("list", flag.ContinueOnError)
		f.SetOutput(io.Discard)
		limit := f.Int("limit", 100, "page limit")
		cursor := f.String("cursor", "", "opaque cursor")
		if e := f.Parse(args[2:]); e != nil || f.NArg() != 0 || *limit < 1 || *limit > 1000 || len(*cursor) > 8192 {
			return "", "", nil, invalid
		}
		if len(args) == 2 {
			return http.MethodGet, base, nil, nil
		}
		q := url.Values{"limit": {strconv.Itoa(*limit)}}
		if *cursor != "" {
			q.Set("cursor", *cursor)
		}
		return http.MethodGet, base + "?" + q.Encode(), nil, nil
	}
	if args[0] == "job" && args[1] == "run" {
		if len(args) < 3 || !jobs.Type(args[2]).Valid() {
			return "", "", nil, fmt.Errorf("invalid job type; supported types: %v", jobs.Types())
		}
		f := flag.NewFlagSet("job run", flag.ContinueOnError)
		f.SetOutput(io.Discard)
		dry := f.Bool("dry-run", false, "dry run")
		revision := f.Uint64("policy-revision", 0, "pinned policy revision")
		params := f.String("params-file", "", "JSON params")
		if e := f.Parse(args[3:]); e != nil || f.NArg() != 0 || *revision > storage.MaxPolicyRevision {
			return "", "", nil, invalid
		}
		doc := json.RawMessage(`{}`)
		if *params != "" {
			file, e := os.Open(*params)
			if e != nil {
				return "", "", nil, errors.New("cannot read params file")
			}
			data, e := io.ReadAll(io.LimitReader(file, 8193))
			file.Close()
			if e != nil || len(data) > 8192 {
				return "", "", nil, errors.New("params exceed 8 KiB or cannot be read")
			}
			var object map[string]json.RawMessage
			if json.Unmarshal(data, &object) != nil || object == nil {
				return "", "", nil, errors.New("params must be a JSON object")
			}
			doc = data
		}
		body, e := json.Marshal(jobs.Create{Type: jobs.Type(args[2]), DryRun: *dry, Params: doc, PolicyRevision: *revision})
		return http.MethodPost, base, body, e
	}
	if len(args) != 3 || !jobs.ValidID(args[2]) {
		return "", "", nil, invalid
	}
	switch {
	case args[0] == "job" && args[1] == "show":
		return http.MethodGet, base + "/" + args[2], nil, nil
	case args[0] == "job" && args[1] == "cancel":
		return http.MethodDelete, base + "/" + args[2], nil, nil
	case args[0] == "node" && args[1] == "drain":
		return http.MethodPost, base + "/" + args[2] + "/drain", []byte(`{}`), nil
	}
	return "", "", nil, adminclient.ErrUnsupported
}
