package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"unicode/utf8"
)

// maxBody bounds every Admin request body (API §6.5.1).
const maxBody = 16 << 10

var idPattern = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// readBody reads at most maxBody bytes of the request body.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, invalid("body exceeds 16 KiB")
		}
		return nil, invalid("body cannot be read")
	}
	return data, nil
}

// object is one strictly parsed JSON object: no duplicate member, no null
// member, nothing after it. Members are checked against the schema of the
// route by the accessors below.
type object map[string]json.RawMessage

func parseObject(data []byte) (object, error) {
	if !utf8.Valid(data) {
		return nil, invalid("body must be UTF-8 JSON")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
		return nil, invalid("body must be a JSON object")
	}
	o := object{}
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return nil, invalid("body is not valid JSON")
		}
		name := tok.(string) // object keys are always strings
		if _, dup := o[name]; dup {
			return nil, invalid("duplicate member %q", name)
		}
		var v json.RawMessage
		if err := d.Decode(&v); err != nil {
			return nil, invalid("body is not valid JSON")
		}
		if string(v) == "null" {
			return nil, invalid("member %q must not be null", name)
		}
		o[name] = v
	}
	if _, err := d.Token(); err != nil {
		return nil, invalid("body is not valid JSON")
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, invalid("unexpected data after the JSON object")
	}
	return o, nil
}

// decodeObject reads the body and rejects members outside allowed, which
// covers privilege elevation fields (owner, system_admin on grants).
func decodeObject(w http.ResponseWriter, r *http.Request, allowed ...string) (object, error) {
	data, err := readBody(w, r)
	if err != nil {
		return nil, err
	}
	o, err := parseObject(data)
	if err != nil {
		return nil, err
	}
	for name := range o {
		known := false
		for _, a := range allowed {
			known = known || a == name
		}
		if !known {
			return nil, invalid("unknown member %q", name)
		}
	}
	return o, nil
}

func (o object) has(name string) bool { _, ok := o[name]; return ok }

// str returns the string member name, def when absent; it must match
// pattern when pattern is set. required makes absence an error.
func (o object) str(name, def string, required bool, pattern *regexp.Regexp) (string, error) {
	raw, ok := o[name]
	if !ok {
		if required {
			return "", invalid("member %q is required", name)
		}
		return def, nil
	}
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", invalid("member %q must be a string", name)
	}
	if pattern != nil && !pattern.MatchString(s) {
		return "", invalid("member %q does not match %s", name, pattern)
	}
	return s, nil
}

func (o object) boolean(name string, def bool) (bool, error) {
	switch string(o[name]) {
	case "":
		return def, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, invalid("member %q must be a boolean", name)
}

// integer returns a JSON integer member in lo..hi (hi <= MaxRevision), def
// when absent. A fraction or an exponent is not an integer.
func (o object) integer(name string, def, lo, hi uint64) (uint64, error) {
	raw, ok := o[name]
	if !ok {
		return def, nil
	}
	n, ok := jsonInteger(raw)
	if !ok || n < lo || n > hi {
		return 0, invalid("member %q must be an integer in %d..%d", name, lo, hi)
	}
	return n, nil
}

// optionalRevision returns expected_revision as Precondition takes it.
func (o object) optionalRevision() (*uint64, error) {
	raw, ok := o["expected_revision"]
	if !ok {
		return nil, nil
	}
	n, ok := jsonInteger(raw)
	if !ok || n > MaxRevision {
		return nil, invalid("member \"expected_revision\" must be an integer in 0..%d", uint64(MaxRevision))
	}
	return &n, nil
}

func jsonInteger(raw []byte) (uint64, bool) {
	if len(raw) == 0 || len(raw) > 1 && raw[0] == '0' {
		return 0, false
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(string(raw), 10, 64)
	return n, err == nil && n <= MaxRevision
}

// query parses the query string: every name must be in allowed and appear
// once.
func query(r *http.Request, allowed ...string) (url.Values, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, invalid("query string is malformed")
	}
	for name, values := range q {
		known := false
		for _, a := range allowed {
			known = known || a == name
		}
		if !known {
			return nil, invalid("unknown query parameter %q", name)
		}
		if len(values) != 1 {
			return nil, invalid("query parameter %q must appear once", name)
		}
	}
	return q, nil
}
