package badger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
)

func TestLocalAuthorityPersistsHistoryAndFencesPreviousProcess(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "metadata")
	now := time.Now().UTC()
	open := func() *Store {
		m, err := Open(Options{Path: path, RequireProducers: true, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := open()
	if err := m.Bootstrap(ctx, Bootstrap{Users: []s.User{{ID: "owner", Active: true}}, Credentials: []s.Credential{{ID: "key", UserID: "owner", Active: true}}}); err != nil {
		t.Fatal(err)
	}
	a, err := m.StartLocalRuntime(ctx, "owner", 1024)
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.Admit(ctx, "key", s.ObjectKey{}, strings.Repeat("a", 32), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.CreateBucket(ctx, s.BucketCreate{Name: "test-bucket", CredentialID: "key", UserID: "owner"}); !errors.Is(err, s.ErrFenced) {
		t.Fatal("strict store accepted unadmitted bucket", err)
	}
	if _, _, err := m.CreateBucket(s.WithMutation(ctx, c), s.BucketCreate{Name: "test-bucket", CredentialID: "key", UserID: "owner"}); err != nil {
		t.Fatal(err)
	}
	mutation, err := m.GetMutation(ctx, c.MutationID)
	if err != nil || mutation.Event.PayloadPointer == "" || mutation.Event.PayloadSHA256 == strings.Repeat("0", 64) {
		t.Fatal("bucket was not journaled by producer", mutation, err)
	}
	payload, err := m.Open(ctx, mutation.Event.PayloadPointer)
	if err != nil {
		t.Fatal(err)
	}
	payloadBytes, err := io.ReadAll(payload)
	payload.Close()
	sum := sha256.Sum256(payloadBytes)
	if err != nil || hex.EncodeToString(sum[:]) != mutation.Event.PayloadSHA256 {
		t.Fatal("retained producer payload hash mismatch", err)
	}
	if _, err := a.Admit(ctx, "key", s.ObjectKey{}, c.MutationID, now.Add(time.Minute)); !errors.Is(err, s.ErrConflict) {
		t.Fatal("one fence reused for two requests", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m = open()
	defer m.Close()
	b, err := m.StartLocalRuntime(ctx, "owner", 9999)
	if err != nil || b.state.History != c.HistoryID || b.Incarnation() == c.Writer.ServiceIncarnation {
		t.Fatal("restart lost history or reused incarnation", err)
	}
	q, err := m.GetQuota(ctx, "owner")
	if err != nil || q.Limit != 1024 {
		t.Fatal("restart overwrote quota", q, err)
	}
	err = m.view(ctx, func(t *db.Txn) error { return m.fence(t, c.Writer) })
	if !errors.Is(err, s.ErrFenced) {
		t.Fatal("old backend authority survived restart", err)
	}
	for _, tc := range []struct {
		credential string
		deadline   time.Time
	}{
		{"wrong", now.Add(time.Minute)}, {"key", now}, {"key", now.Add(31 * time.Minute)},
	} {
		if _, err := b.Admit(ctx, tc.credential, s.ObjectKey{}, strings.Repeat("b", 32), tc.deadline); err == nil {
			t.Fatal("bad admission accepted", tc)
		}
	}
}

func TestLocalRuntimeRefusesUnknownAccounting(t *testing.T) {
	m, err := Open(Options{Path: t.TempDir(), RequireProducers: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := context.Background()
	if err := m.Bootstrap(ctx, Bootstrap{Users: []s.User{{ID: "owner", Active: true}}}); err != nil {
		t.Fatal(err)
	}
	if err := m.update(ctx, func(t *db.Txn) error { return put(t, key("heads", "old"), s.ObjectHead{Size: 42}) }); err != nil {
		t.Fatal(err)
	}
	if _, err := m.StartLocalRuntime(ctx, "owner", 1024); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal("populated legacy store was silently reset", err)
	}
	if _, err := m.GetQuota(ctx, "owner"); !errors.Is(err, s.ErrNotFound) {
		t.Fatal("failed upgrade wrote partial quota", err)
	}
}
