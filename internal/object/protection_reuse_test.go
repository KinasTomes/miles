package object

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/storage"
)

func TestRepeatedPUTAdvancingTimeReusesWarmAndExtendsProtection(t *testing.T) {
	f := newServiceFixture(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte("repeat-backup"), 1000)
	f.put(t, ctx, "backup", body)
	object := storage.ObjectKey{Bucket: f.bucket.Name, BucketID: f.bucket.ID, Key: "backup"}
	head, e := f.metadata.GetObject(ctx, object)
	if e != nil {
		t.Fatal(e)
	}
	pages, e := f.metadata.GetManifest(ctx, head.ManifestID)
	if e != nil || len(pages) != 1 || len(pages[0].Chunks) != 1 {
		t.Fatal(pages, e)
	}
	key := pages[0].Chunks[0].Key
	rows, e := f.metadata.GetChunks(ctx, []storage.ChunkKey{key}, true)
	if e != nil {
		t.Fatal(e)
	}
	original := rows[key]
	packs, e := f.metadata.GetPacks(ctx, []storage.PackRef{original.Location.Pack})
	if e != nil {
		t.Fatal(e)
	}
	old := packs[original.Location.Pack]
	f.physical.mu.Lock()
	objectsBefore := len(f.physical.objects)
	f.physical.mu.Unlock()
	if e := f.options.Clock.(*clock.Virtual).Advance(time.Second); e != nil {
		t.Fatal(e)
	}
	f.put(t, ctx, "backup", body)
	rows, e = f.metadata.GetChunks(ctx, []storage.ChunkKey{key}, true)
	if e != nil {
		t.Fatal(e)
	}
	if rows[key] != original {
		t.Fatalf("repeated PUT changed canonical binding: before=%+v after=%+v", original, rows[key])
	}
	packs, e = f.metadata.GetPacks(ctx, []storage.PackRef{original.Location.Pack})
	if e != nil {
		t.Fatal(e)
	}
	updated := packs[original.Location.Pack]
	required := f.options.Clock.Now().WallUTC.Add(f.options.RequestTimeout + 15*time.Minute)
	if updated.ProtectedUntil.Before(required) || !updated.ProtectedUntil.After(old.ProtectedUntil) {
		t.Fatal("durable protection not extended", updated.ProtectedUntil, old.ProtectedUntil, required)
	}
	if updated.State != "Warm" || updated.Class != storage.Standard {
		t.Fatal("pack changed", updated)
	}
	f.physical.mu.Lock()
	objectsAfter := len(f.physical.objects)
	f.physical.mu.Unlock()
	if objectsAfter != objectsBefore {
		t.Fatal("repeated PUT uploaded a new pack/sidecar", objectsBefore, objectsAfter)
	}
	_, got := f.get(t, ctx, "backup", nil)
	if !bytes.Equal(got, body) {
		t.Fatal("read body changed")
	}
}

func TestProtectionExtensionDoesNotInvalidateAdmittedGET(t *testing.T) {
	f := newServiceFixture(t)
	ctx := context.Background()
	v, _ := config.NewPolicyValidator(config.DSGPolicyLimits())
	f.options.BucketPolicy = func(id string) (config.BucketSnapshot, error) {
		p := config.DefaultCachePolicy()
		p.ReadaheadChunks = 0
		return v.NewBucketSnapshot(id, 1, p)
	}
	f.options.MaxOutstanding = 1
	var e error
	f.service, e = New(f.options)
	if e != nil {
		t.Fatal(e)
	}
	body := bytes.Repeat([]byte("shared-read"), 5000)
	f.put(t, ctx, "backup", body)
	entered := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	joined := false
	defer func() {
		once.Do(func() { close(resume) })
		if !joined {
			<-done
		}
	}()
	go func() {
		var output bytes.Buffer
		first := true
		_, err := f.service.GetObject(ctx, GetRequest{Bucket: f.bucket.Name, BucketID: f.bucket.ID, Key: "backup", CredentialID: "owner-key", Emit: func(_ context.Context, b []byte) error {
			if first {
				first = false
				close(entered)
				<-resume
			}
			_, err := output.Write(b)
			return err
		}})
		done <- result{output.Bytes(), err}
	}()
	select {
	case <-entered:
	case r := <-done:
		joined = true
		t.Fatal("GET failed before barrier", r.err)
	}
	if err := f.options.Clock.(*clock.Virtual).Advance(time.Second); err != nil {
		t.Fatal(err)
	}
	f.put(t, ctx, "backup", body)
	once.Do(func() { close(resume) })
	r := <-done
	joined = true
	if r.err != nil || !bytes.Equal(r.body, body) {
		t.Fatal("protection-only extension aborted admitted GET", r.err, len(r.body), len(body))
	}
}
