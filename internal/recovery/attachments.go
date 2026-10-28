package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/KinasTomes/miles/internal/manifest"
	"github.com/KinasTomes/miles/internal/recovery/format"
	s "github.com/KinasTomes/miles/internal/storage"
)

type AttachmentReference = format.PayloadDependency
type attachmentPage struct {
	Number   uint32
	Whole    format.PageReference
	Segments []Reference
}
type attachmentGroup struct {
	Version uint32
	Number  uint32
	Pages   []attachmentPage
}
type attachmentIndex struct {
	Version        uint32
	History, Event string
	Epoch          uint64
	Kind           byte
	Owner, Domain  string
	Length         int64
	SHA256         string
	PageCount      uint32
	Groups         []AttachmentReference
}

func readReference(ctx context.Context, source PayloadSource, p AttachmentReference) ([]byte, error) {
	if p.Reference.Validate() != nil || p.Key == "" {
		return nil, s.ErrInvalid
	}
	r, err := source.Open(ctx, p.Key)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, s.ErrUnavailable
	}
	body, readErr := io.ReadAll(io.LimitReader(r, MaxMetadataBytes+1))
	closeErr := r.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(body) != p.Reference.Length || digest(body) != p.Reference.SHA256 {
		return nil, s.ErrInvalid
	}
	return body, nil
}
func (w Exporter) exportAttachment(ctx context.Context, event s.OutboxEvent, dep AttachmentReference, ordinal int) (AttachmentReference, error) {
	var zero AttachmentReference
	body, err := readReference(ctx, w.Payloads, dep)
	if err != nil {
		return zero, err
	}
	var source format.Attachment
	if Decode(body, &source) != nil || source.Version != 1 || len(source.Pages) == 0 || len(source.Pages) > 8 || source.Kind != 1 && source.Kind != 3 && source.Kind != 4 {
		return zero, s.ErrInvalid
	}
	index := attachmentIndex{Version: 1, History: event.HistoryID, Event: event.ID, Epoch: event.Epoch, Kind: source.Kind, Owner: source.Owner, Domain: source.Domain, Length: source.Length, SHA256: source.SHA256, Groups: []AttachmentReference{}}
	prefix := eventRoot(event) + "dependencies/" + strconv.Itoa(ordinal) + "/"
	var number uint32
	for g, refs := range source.Pages {
		if len(refs) == 0 || len(refs) > 128 || g < len(source.Pages)-1 && len(refs) != 128 {
			return zero, s.ErrInvalid
		}
		group := attachmentGroup{Version: 1, Number: uint32(g), Pages: []attachmentPage{}}
		for _, ref := range refs {
			if number >= 1024 || ref.Length <= 0 || ref.Length > manifest.MaxPageBytes || !isHex(ref.SHA256, 64) {
				return zero, s.ErrInvalid
			}
			uri := fmt.Sprintf("attachment/%d/%s/%d", source.Kind, source.Owner, number)
			r, err := w.Payloads.Open(ctx, uri)
			if err != nil {
				return zero, err
			}
			if r == nil {
				return zero, s.ErrUnavailable
			}
			raw, readErr := io.ReadAll(io.LimitReader(r, manifest.MaxPageBytes+1))
			closeErr := r.Close()
			if readErr != nil {
				return zero, readErr
			}
			if closeErr != nil {
				return zero, closeErr
			}
			if len(raw) != ref.Length || digest(raw) != ref.SHA256 {
				return zero, s.ErrInvalid
			}
			if err := validateAttachmentPage(index, number, raw); err != nil {
				return zero, err
			}
			page := attachmentPage{Number: number, Whole: ref, Segments: []Reference{}}
			for off, segment := 0, 0; off < len(raw); segment++ {
				end := min(off+128<<10, len(raw))
				chunk := raw[off:end]
				key := prefix + fmt.Sprintf("pages/%d/%d.bin", number, segment)
				if err := putVerified(ctx, w.Objects, key, chunk); err != nil {
					return zero, err
				}
				page.Segments = append(page.Segments, Reference{Length: len(chunk), SHA256: digest(chunk)})
				off = end
			}
			group.Pages = append(group.Pages, page)
			number++
		}
		encoded, err := Encode(group)
		if err != nil {
			return zero, err
		}
		key := prefix + fmt.Sprintf("groups/%d.json", g)
		if err = putVerified(ctx, w.Objects, key, encoded); err != nil {
			return zero, err
		}
		index.Groups = append(index.Groups, AttachmentReference{Key: key, Reference: refBytes(encoded)})
	}
	index.PageCount = number
	encoded, err := Encode(index)
	if err != nil {
		return zero, err
	}
	key := prefix + "index.json"
	if err = putVerified(ctx, w.Objects, key, encoded); err != nil {
		return zero, err
	}
	result := AttachmentReference{Key: key, Reference: refBytes(encoded)}
	if err = VerifyAttachment(ctx, w.Objects, result, event); err != nil {
		return zero, err
	}
	return result, nil
}
func refBytes(body []byte) Reference { return Reference{Length: len(body), SHA256: digest(body)} }
func validateAttachmentPage(index attachmentIndex, n uint32, body []byte) error {
	if index.Kind == 1 {
		page, err := manifest.Decode(body, 1, index.Owner, n)
		if err != nil {
			return err
		}
		if page.Domain != index.Domain {
			return s.ErrInvalid
		}
		return nil
	}
	_, err := manifest.DecodeCompletionPage(body, index.Kind, index.Owner, n)
	return err
}

// VerifyAttachment reads only exported immutable artifacts. It does not consult
// the live metadata source and therefore proves the serialized page dependency
// survives loss of that source. One binary page plus one bounded group is held.
func VerifyAttachment(ctx context.Context, objects ImmutableStore, root AttachmentReference, event s.OutboxEvent) error {
	if !strings.HasSuffix(root.Key, "/index.json") {
		return s.ErrInvalid
	}
	read := func(ref AttachmentReference) ([]byte, error) {
		if ref.Reference.Validate() != nil {
			return nil, s.ErrInvalid
		}
		b, err := objects.Read(ctx, ref.Key)
		if err != nil {
			return nil, err
		}
		if len(b) != ref.Reference.Length || digest(b) != ref.Reference.SHA256 {
			return nil, s.ErrInvalid
		}
		return b, nil
	}
	body, err := read(root)
	if err != nil {
		return err
	}
	var index attachmentIndex
	if Decode(body, &index) != nil || index.Version != 1 || index.History != event.HistoryID || index.Event != event.ID || index.Epoch != event.Epoch || index.PageCount == 0 || index.PageCount > 1024 || len(index.Groups) == 0 || len(index.Groups) > 8 {
		return s.ErrInvalid
	}
	prefix := root.Key[:len(root.Key)-len("index.json")]
	var pageNo uint32
	var length int64
	streamHash := sha256.New()
	var objectOffset int64
	for i, ref := range index.Groups {
		if ref.Key != prefix+fmt.Sprintf("groups/%d.json", i) {
			return s.ErrInvalid
		}
		body, err := read(ref)
		if err != nil {
			return err
		}
		var group attachmentGroup
		if Decode(body, &group) != nil || group.Version != 1 || int(group.Number) != i || len(group.Pages) == 0 || len(group.Pages) > 128 {
			return s.ErrInvalid
		}
		for _, page := range group.Pages {
			if page.Number != pageNo || len(page.Segments) == 0 || len(page.Segments) > 3 {
				return s.ErrInvalid
			}
			var raw bytes.Buffer
			for j, segment := range page.Segments {
				if segment.Length > 128<<10 || j < len(page.Segments)-1 && segment.Length != 128<<10 {
					return s.ErrInvalid
				}
				b, err := read(AttachmentReference{Key: prefix + fmt.Sprintf("pages/%d/%d.bin", pageNo, j), Reference: segment})
				if err != nil {
					return err
				}
				raw.Write(b)
			}
			data := raw.Bytes()
			if len(data) != page.Whole.Length || digest(data) != page.Whole.SHA256 || validateAttachmentPage(index, pageNo, data) != nil {
				return s.ErrInvalid
			}
			if index.Kind == 1 {
				p, err := manifest.Decode(data, 1, index.Owner, pageNo)
				if err != nil {
					return err
				}
				for _, r := range p.Chunks {
					if r.LogicalOffset != objectOffset {
						return s.ErrInvalid
					}
					objectOffset += r.RawBytes
				}
			}
			if index.Kind == 3 || index.Kind == 4 {
				payload, err := manifest.DecodeCompletionPage(data, index.Kind, index.Owner, pageNo)
				if err != nil {
					return err
				}
				streamHash.Write(payload)
				length += int64(len(payload))
			}
			pageNo++
		}
	}
	if pageNo != index.PageCount {
		return s.ErrInvalid
	}
	if index.Kind == 1 && objectOffset != index.Length {
		return s.ErrInvalid
	}
	if index.Kind != 1 && (length != index.Length || hex.EncodeToString(streamHash.Sum(nil)) != index.SHA256) {
		return s.ErrInvalid
	}
	return nil
}
