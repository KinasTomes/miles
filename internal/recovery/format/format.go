// Package format is the dependency-free bounded recovery wire codec.
package format

import (
	"bytes"
	"encoding/json"
	s "github.com/KinasTomes/miles/internal/storage"
	"io"
	"reflect"
	"strconv"
	"unicode/utf8"
)

const MaxMetadataBytes = 256 << 10
const MaxFanout = 256
const maxInteger = 1<<53 - 1

type Reference struct {
	Length int    `json:"length"`
	SHA256 string `json:"sha256"`
}
type Effect struct {
	Deleted  bool   `json:"deleted"`
	Key      string `json:"key"`
	Revision uint64 `json:"revision"`
	Value    string `json:"value"`
}
type EventPage struct {
	Effects []Effect `json:"effects"`
	Epoch   uint64   `json:"epoch"`
	Event   string   `json:"event"`
	History string   `json:"history"`
	Number  int      `json:"number"`
	Version int      `json:"r2_version"`
}

// Input is a retained prototype bundle. Binary FORMAT attachments are not
// embedded in this schema. Full producer projections remain an integration gate.
type Input struct {
	Pages        []EventPage         `json:"pages"`
	Dependencies []PayloadDependency `json:"dependencies,omitempty"`
}

type PayloadDependency struct {
	Key       string    `json:"key"`
	Reference Reference `json:"reference"`
}
type PageReference struct {
	Length int    `json:"length"`
	SHA256 string `json:"sha256"`
}
type Attachment struct {
	Version       uint32 `json:"version"`
	Kind          byte   `json:"kind"`
	Owner, Domain string
	Length        int64
	SHA256        string
	Pages         [][]PageReference
}

type ProducerScope struct {
	History, Logical, Mutation, Entity, Identity, Action string
	Epoch                                                uint64
}
type Projection struct {
	Version uint32
	Entity  string
	Value   json.RawMessage
}

// Encode supports canonical Unicode strings and nonnegative safe integers.
// Unsupported null/fractional/negative number forms still fail closed.
func Encode(v any) ([]byte, error) {
	if !validStrings(reflect.ValueOf(v)) {
		return nil, s.ErrInvalid
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, s.ErrInvalid
	}
	var tree any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&tree) != nil || !subset(tree, 0) {
		return nil, s.ErrInvalid
	}
	b = canonicalSubset(tree)
	if len(b) == 0 || len(b) > MaxMetadataBytes {
		return nil, s.ErrInvalid
	}
	return b, nil
}
func subset(v any, depth int) bool {
	if depth > 8 {
		return false
	}
	switch x := v.(type) {
	case string:
		return utf8.ValidString(x)
	case bool:
		return true
	case json.Number:
		n, err := strconv.ParseUint(string(x), 10, 53)
		return err == nil && n <= maxInteger && strconv.FormatUint(n, 10) == string(x)
	case []any:
		if len(x) > MaxFanout {
			return false
		}
		for _, a := range x {
			if !subset(a, depth+1) {
				return false
			}
		}
		return true
	case map[string]any:
		if len(x) > MaxFanout {
			return false
		}
		for k, a := range x {
			if !subset(k, depth+1) || !subset(a, depth+1) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
func Decode(b []byte, out any) error {
	if len(b) == 0 || len(b) > MaxMetadataBytes {
		return s.ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return s.ErrInvalid
	}
	canonical, err := Encode(out)
	if err != nil || !bytes.Equal(b, canonical) {
		return s.ErrInvalid
	}
	return nil
}

func (r Reference) Validate() error {
	if r.Length <= 0 || r.Length > MaxMetadataBytes || len(r.SHA256) != 64 {
		return s.ErrInvalid
	}
	for _, c := range r.SHA256 {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return s.ErrInvalid
		}
	}
	return nil
}
