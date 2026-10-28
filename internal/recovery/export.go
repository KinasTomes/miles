// Package recovery contains the bounded W03 exporter prototype. It does not
// close epochs, grant worker authority, publish seals or enable recovery.
package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/KinasTomes/miles/internal/recovery/format"
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

type Reference = format.Reference
type Effect = format.Effect
type EventPage = format.EventPage
type Input = format.Input

type Descriptor struct {
	Dependencies []AttachmentReference `json:"dependencies,omitempty"`
	Action       string                `json:"action"`
	Entity       string                `json:"entity"`
	Epoch        uint64                `json:"epoch"`
	History      string                `json:"history"`
	ID           string                `json:"id"`
	Logical      string                `json:"logical_op"`
	Mutation     string                `json:"mutation"`
	Pages        []Reference           `json:"pages"`
	Version      int                   `json:"r2_version"`
	Source       s.OutboxEvent         `json:"source"`
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

func Encode(v any) ([]byte, error)   { return format.Encode(v) }
func Decode(b []byte, out any) error { return format.Decode(b, out) }

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
			if effect.Key == "ProducerScope/"+hex.EncodeToString([]byte(e.OpID)) {
				raw, err := base64.StdEncoding.Strict().DecodeString(effect.Value)
				if err != nil {
					return s.ErrInvalid
				}
				var projection format.Projection
				if json.Unmarshal(raw, &projection) != nil || projection.Version != 1 || projection.Entity != "ProducerScope" {
					return s.ErrInvalid
				}
				var scope format.ProducerScope
				if json.Unmarshal(projection.Value, &scope) != nil || scope.History != e.HistoryID || scope.Mutation != e.OpID || scope.Epoch != e.Epoch || scope.Entity != e.Entity || scope.Action != e.Action || !isHex(scope.Logical, 32) {
					return s.ErrInvalid
				}
				d.Logical = scope.Logical
				identity := e.EntityID
				if e.Entity == "Objects" {
					body, err := json.Marshal(e.Object)
					if err != nil {
						return err
					}
					identity = string(body)
				}
				if scope.Identity != identity {
					return s.ErrInvalid
				}
			}
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
	for i, dep := range input.Dependencies {
		exported, err := w.exportAttachment(ctx, e, dep, i)
		if err != nil {
			return err
		}
		d.Dependencies = append(d.Dependencies, exported)
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
