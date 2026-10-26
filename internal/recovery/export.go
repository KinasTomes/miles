// Package recovery contains the bounded W03 exporter prototype. It does not
// close epochs, grant worker authority, publish seals or enable recovery.
package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	s "github.com/KinasTomes/miles/internal/storage"
)

const MaxMetadataBytes = 256 << 10
const MaxFanout = 256
const maxInteger = 1<<53 - 1

// ImmutableStore must conditional-create and read back exact bytes. It cannot
// overwrite existing keys, even when their contents fail verification.
type ImmutableStore interface {
	Create(context.Context, string, []byte) error
	Read(context.Context, string) ([]byte, error)
}

// PayloadSource reads retained, immutable producer inputs, NOT current heads.
// Open must not silently substitute a newer revision for a missing input.
type PayloadSource interface {
	Open(context.Context, string) (io.ReadCloser, error)
}

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
	Pages []EventPage `json:"pages"`
}
type Descriptor struct {
	Action   string        `json:"action"`
	Entity   string        `json:"entity"`
	Epoch    uint64        `json:"epoch"`
	History  string        `json:"history"`
	ID       string        `json:"id"`
	Logical  string        `json:"logical_op"`
	Mutation string        `json:"mutation"`
	Pages    []Reference   `json:"pages"`
	Version  int           `json:"r2_version"`
	Source   s.OutboxEvent `json:"source"`
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func ref(b []byte) Reference { return Reference{len(b), digest(b)} }
func isHex(v string, n int) bool {
	if len(v) != n {
		return false
	}
	for _, c := range v {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Encode implements only the explicit ASCII/nonnegative-integer JCS subset of
// the W02 fixtures. Unsupported Unicode, null and number forms fail closed.
// This deliberately makes no full RFC8785 conformance or format-freeze claim.
func Encode(v any) ([]byte, error) {
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
	b, err = json.Marshal(tree)
	if err != nil || len(b) == 0 || len(b) > MaxMetadataBytes {
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
		for _, c := range x {
			if c < 32 || c > 126 || c == '<' || c == '>' || c == '&' {
				return false
			}
		}
		return true
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

type Exporter struct {
	Metadata s.OutboxExportStore
	Payloads PayloadSource
	Objects  ImmutableStore
}

func eventRoot(e s.OutboxEvent) string {
	return "journal/" + e.HistoryID + "/" + strconv.FormatUint(e.Epoch, 10) + "/" + e.ID + "/"
}

func (w Exporter) Export(ctx context.Context, id string) error {
	if w.Metadata == nil || w.Payloads == nil || w.Objects == nil {
		return s.ErrInvalid
	}
	e, err := w.Metadata.GetOutbox(ctx, id)
	if err != nil {
		return err
	}
	if e.Validate() != nil || e.Epoch > maxInteger {
		return s.ErrInvalid
	}
	r, err := w.Payloads.Open(ctx, e.PayloadPointer)
	if err != nil {
		return err
	}
	if r == nil {
		return s.ErrInvalid
	}
	b, readErr := io.ReadAll(io.LimitReader(r, MaxMetadataBytes+1))
	closeErr := r.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if len(b) > MaxMetadataBytes || digest(b) != e.PayloadSHA256 {
		return fmt.Errorf("%w: retained payload digest", s.ErrInvalid)
	}
	var input Input
	if Decode(b, &input) != nil || len(input.Pages) == 0 {
		return s.ErrInvalid
	}
	d := Descriptor{Action: e.Action, Entity: e.Entity, Epoch: e.Epoch, History: e.HistoryID, ID: e.ID, Logical: e.OpID, Mutation: e.OpID, Pages: []Reference{}, Version: 1, Source: e}
	// Validate the whole bundle before creating any artifacts.
	pages := make([][]byte, len(input.Pages))
	seen := make(map[string]bool)
	for i, p := range input.Pages {
		if p.Version != 1 || p.Event != e.ID || p.History != e.HistoryID || p.Epoch != e.Epoch || p.Number != i || len(p.Effects) == 0 {
			return s.ErrInvalid
		}
		for _, effect := range p.Effects {
			if effect.Key == "" || effect.Revision == 0 || effect.Revision > maxInteger || seen[effect.Key] || effect.Deleted && effect.Value != "" {
				return s.ErrInvalid
			}
			seen[effect.Key] = true
		}
		pages[i], err = Encode(p)
		if err != nil {
			return err
		}
		d.Pages = append(d.Pages, ref(pages[i]))
	}
	for i, body := range pages {
		if err := putVerified(ctx, w.Objects, eventRoot(e)+"pages/"+strconv.Itoa(i)+".json", body); err != nil {
			return err
		}
	}
	body, err := Encode(d)
	if err != nil {
		return err
	}
	key := eventRoot(e) + "descriptor.json"
	if err = putVerified(ctx, w.Objects, key, body); err != nil {
		return err
	}
	return w.Metadata.MarkOutboxExported(ctx, s.OutboxExport{Event: e, DescriptorKey: key, DescriptorSHA256: digest(body)})
}
func putVerified(ctx context.Context, sink ImmutableStore, key string, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Even a successful PUT or previous export flag is not a readback proof.
	if err := sink.Create(ctx, key, body); err != nil {
		return err
	}
	got, err := sink.Read(ctx, key)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, body) {
		return fmt.Errorf("%w: immutable artifact %s", s.ErrConflict, key)
	}
	return nil
}

// RunPage resumes with Next. The caller schedules retries; a failed event never
// advances the cursor past that event. A completed open scan is not a cut.
func (w Exporter) RunPage(ctx context.Context, q s.OutboxQuery) (string, error) {
	if w.Metadata == nil {
		return q.After, s.ErrInvalid
	}
	page, err := w.Metadata.ListOutbox(ctx, q)
	if err != nil {
		return q.After, err
	}
	after := q.After
	for _, e := range page.Events {
		if err := w.Export(ctx, e.ID); err != nil {
			return after, err
		}
		after = e.ID
	}
	return page.Next, nil
}
