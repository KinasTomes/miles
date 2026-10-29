// Package app wires process lifecycle and adapters. It does not implement S3 handlers or R1.
package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

// Dependencies use the existing storage ports. Probe is supplied by their wiring,
// including validated clock readiness; health handlers never invent clock proof.
type Dependencies struct {
	Metadata  storage.MetadataStore
	Packs     storage.PackStore
	Cache     storage.Cache
	Objects   *object.Service // real Warm service in local/benchmark mode; absent in fixture/runtime modes
	Probe     func(context.Context) error
	Close     func() error
	S3Handler func(*Admission) http.Handler
	// DevCredentials are the SigV4 test credentials dev provisioning minted
	// into the selected development store (API v2 3.5 format, bound to DevUser,
	// no admin rights). Dev/local only; runtime leaves it empty.
	DevCredentials []DevCredential
}

var ErrRuntimeUnavailable = errors.New("runtime storage and validated clock adapters are not configured")

// Bootstrap requires explicit dev/local/benchmark mode for development identities/storage.
// Runtime stays unready until the metadata/storage/clock owners wire real adapters.
func Bootstrap(c config.Startup) (Dependencies, error) {
	if err := c.Validate(); err != nil {
		return Dependencies{}, err
	}
	if c.Mode == "local" || c.Mode == "benchmark" {
		return bootstrapLocal(c)
	}
	if c.Mode != "dev" {
		return Dependencies{Probe: func(context.Context) error { return ErrRuntimeUnavailable }}, nil
	}
	const bucket = "miles-dev-probe"
	ref := storage.PackRef{ID: "dev-probe", Generation: "1"}
	body := []byte("miles-dev-probe")
	// Dev provisioning mints one SigV4 test credential per process from the
	// entropy source; nothing secret is read from or written to the repo.
	user, credential, minted, err := devFixtureIdentity(rand.Reader)
	if err != nil {
		return Dependencies{}, err
	}
	m := fake.NewMetadata(fake.Seed{Buckets: []storage.Bucket{{Name: bucket, ID: "dev-probe", OwnerID: user.ID, Domain: user.DefaultDomain, Active: true, Revision: 1, CreatedAt: time.Now().UTC()}}, Users: []storage.User{user}, Credentials: []storage.Credential{credential}})
	p := fake.NewPacks(nil)
	cache := fake.NewCache()
	ctx := context.Background()
	if err := p.PutPack(ctx, storage.PackInfo{Version: storage.ModelVersion, Ref: ref, Size: int64(len(body)), Class: storage.Standard, State: "Warm"}, bytes.NewReader(body)); err != nil {
		return Dependencies{}, err
	}
	key := storage.CacheKey{Pack: ref, Range: storage.ByteRange{Offset: 0, Length: int64(len(body))}}
	if err := cache.Put(ctx, key, body); err != nil {
		return Dependencies{}, err
	}
	probe := func(ctx context.Context) error {
		if _, err := m.GetBucket(ctx, bucket); err != nil {
			return fmt.Errorf("dev metadata: %w", err)
		}
		if _, err := p.HeadPack(ctx, ref); err != nil {
			return fmt.Errorf("dev pack: %w", err)
		}
		got, hit, err := cache.Get(ctx, key)
		if err != nil || !hit || !bytes.Equal(got, body) {
			return errors.New("dev cache probe unavailable")
		}
		return ctx.Err()
	}
	return Dependencies{Metadata: m, Packs: coordination.ProtectPackStore(p), Cache: cache, Probe: probe, DevCredentials: []DevCredential{minted}}, nil
}
