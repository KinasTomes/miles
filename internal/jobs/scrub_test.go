package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

func createTestPack(t *testing.T, store storage.PackStore, ref storage.PackRef, class storage.StorageClass, state string, chunkContent []byte) storage.PackInfo {
	t.Helper()
	builder, err := pack.NewBuilder(ref)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(chunkContent)
	hexHash := hex.EncodeToString(sum[:])
	err = builder.Add(pack.Blob{
		Key:       storage.ChunkKey{Domain: "default", Hash: hexHash},
		RawLength: uint32(len(chunkContent)),
		Stored:    chunkContent,
		Codec:     "raw",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, sidecar, _, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	rawInfo := storage.PackInfo{
		Version: storage.ModelVersion,
		Ref:     ref,
		Size:    int64(len(data)),
		Class:   storage.Standard,
		State:   "Warm",
	}
	if err := store.PutPack(context.Background(), rawInfo, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := store.PutSidecar(context.Background(), ref, int64(len(sidecar)), bytes.NewReader(sidecar)); err != nil {
		t.Fatal(err)
	}
	return storage.PackInfo{
		Version: storage.ModelVersion,
		Ref:     ref,
		Size:    int64(len(data)),
		Class:   class,
		State:   state,
	}
}

func createCorruptChunkPack(t *testing.T, store storage.PackStore, ref storage.PackRef) storage.PackInfo {
	t.Helper()
	builder, err := pack.NewBuilder(ref)
	if err != nil {
		t.Fatal(err)
	}
	goodContent := []byte("original chunk data 123456")
	sum := sha256.Sum256(goodContent)
	hexHash := hex.EncodeToString(sum[:])
	err = builder.Add(pack.Blob{
		Key:       storage.ChunkKey{Domain: "default", Hash: hexHash},
		RawLength: uint32(len(goodContent)),
		Stored:    goodContent,
		Codec:     "raw",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, sidecar, entries, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	// Tamper with the chunk byte at entries[0].Offset
	tamperedData := bytes.Clone(data)
	tamperedData[entries[0].Offset] ^= 0xFF

	rawInfo := storage.PackInfo{
		Version: storage.ModelVersion,
		Ref:     ref,
		Size:    int64(len(tamperedData)),
		Class:   storage.Standard,
		State:   "Warm",
	}
	if err := store.PutPack(context.Background(), rawInfo, bytes.NewReader(tamperedData)); err != nil {
		t.Fatal(err)
	}
	if err := store.PutSidecar(context.Background(), ref, int64(len(sidecar)), bytes.NewReader(sidecar)); err != nil {
		t.Fatal(err)
	}
	return rawInfo
}

func TestScrubWorkerDryRunAndCancel(t *testing.T) {
	worker := &ScrubWorker{}
	exec := worker.Executor()

	step, err := exec.Step(context.Background(), Job{
		DryRun: true,
	}, config.GlobalSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if !step.Done || !step.Settled {
		t.Fatalf("expected done and settled, got %+v", step)
	}
	var res ScrubResult
	if err := json.Unmarshal(step.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.SkipReasons["dry_run_or_cancelled"] != 1 {
		t.Fatalf("expected dry_run_or_cancelled, got %+v", res)
	}
}

func TestScrubWorkerValidateParams(t *testing.T) {
	worker := &ScrubWorker{}
	exec := worker.Executor()

	if err := exec.Validate(json.RawMessage(`{"limit": 50}`)); err != nil {
		t.Fatalf("valid params rejected: %v", err)
	}
	if err := exec.Validate(json.RawMessage(`{"limit": 50, "state": "Warm"}`)); err != nil {
		t.Fatalf("valid state Warm rejected: %v", err)
	}
	if err := exec.Validate(json.RawMessage(`{"limit": 50, "state": "Cold"}`)); err != nil {
		t.Fatalf("valid state Cold rejected: %v", err)
	}
	if err := exec.Validate(json.RawMessage(`{"limit": 50, "state": "Restored"}`)); err != nil {
		t.Fatalf("valid state Restored rejected: %v", err)
	}
	if err := exec.Validate(json.RawMessage(`{"limit": -1}`)); err == nil {
		t.Fatal("negative limit should be rejected")
	}
	if err := exec.Validate(json.RawMessage(`{"limit": 1001}`)); err == nil {
		t.Fatal("limit > 1000 should be rejected")
	}
	if err := exec.Validate(json.RawMessage(`{"state": "invalid_state"}`)); err == nil {
		t.Fatal("invalid state should be rejected")
	}
}

func TestScrubWorkerMissingStoresHandledGracefully(t *testing.T) {
	worker := &ScrubWorker{Store: nil, Packs: nil}
	exec := worker.Executor()

	step, err := exec.Step(context.Background(), Job{}, config.GlobalSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if !step.Done || !step.Settled {
		t.Fatalf("expected step to finish, got %+v", step)
	}
	if step.ErrorCode != "store_unavailable" {
		t.Fatalf("expected ErrorCode store_unavailable, got %q", step.ErrorCode)
	}
	var res ScrubResult
	if err := json.Unmarshal(step.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.SkipReasons["missing_store_or_packs"] != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestScrubWorkerValidPackSuccess(t *testing.T) {
	meta := fake.NewMetadata(fake.Seed{})
	packStore := fake.NewPacks(nil)
	ref := storage.PackRef{ID: "pack-valid-1", Generation: "gen-1"}
	info := createTestPack(t, packStore, ref, storage.Standard, "Warm", []byte("valid chunk body for test"))
	meta.SetPack(info)

	worker := &ScrubWorker{Store: meta, Packs: packStore}
	exec := worker.Executor()

	step, err := exec.Step(context.Background(), Job{}, config.GlobalSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if !step.Done || !step.Settled {
		t.Fatalf("expected step to be done and settled, got %+v", step)
	}
	if step.ErrorCode != "" {
		t.Fatalf("expected empty ErrorCode on success, got %q", step.ErrorCode)
	}

	var res ScrubResult
	if err := json.Unmarshal(step.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.Candidates != 1 || res.Checked != 1 || res.Corrupt != 0 || res.Skipped != 0 {
		t.Fatalf("unexpected scrub result: %+v", res)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("expected 0 errors, got %v", res.Errors)
	}
	if res.Partial {
		t.Fatal("expected partial to be false")
	}
}

func TestScrubWorkerCorruptionDetected(t *testing.T) {
	t.Run("chunk hash mismatch", func(t *testing.T) {
		meta := fake.NewMetadata(fake.Seed{})
		packStore := fake.NewPacks(nil)
		ref := storage.PackRef{ID: "pack-corrupt-chunk", Generation: "gen-1"}
		info := createCorruptChunkPack(t, packStore, ref)
		meta.SetPack(info)

		worker := &ScrubWorker{Store: meta, Packs: packStore}
		exec := worker.Executor()

		step, err := exec.Step(context.Background(), Job{}, config.GlobalSnapshot{})
		if err != nil {
			t.Fatal(err)
		}
		if !step.Done || !step.Settled {
			t.Fatalf("expected step done and settled, got %+v", step)
		}
		if step.ErrorCode != "corruption_detected" {
			t.Fatalf("expected ErrorCode corruption_detected, got %q", step.ErrorCode)
		}

		var res ScrubResult
		if err := json.Unmarshal(step.Result, &res); err != nil {
			t.Fatal(err)
		}
		if res.Candidates != 1 || res.Corrupt != 1 || res.Checked != 0 {
			t.Fatalf("expected 1 corrupt, got %+v", res)
		}
		if len(res.Errors) == 0 {
			t.Fatal("expected error details in result")
		}
	})

	t.Run("footer read error", func(t *testing.T) {
		meta := fake.NewMetadata(fake.Seed{})
		packStore := fake.NewPacks(nil)
		ref := storage.PackRef{ID: "pack-corrupt-footer", Generation: "gen-1"}
		badBytes := bytes.Repeat([]byte{0xAA}, 200)
		info := storage.PackInfo{
			Version: storage.ModelVersion,
			Ref:     ref,
			Size:    int64(len(badBytes)),
			Class:   storage.Standard,
			State:   "Warm",
		}
		if err := packStore.PutPack(context.Background(), info, bytes.NewReader(badBytes)); err != nil {
			t.Fatal(err)
		}
		meta.SetPack(info)

		worker := &ScrubWorker{Store: meta, Packs: packStore}
		exec := worker.Executor()

		step, err := exec.Step(context.Background(), Job{}, config.GlobalSnapshot{})
		if err != nil {
			t.Fatal(err)
		}
		if step.ErrorCode != "corruption_detected" {
			t.Fatalf("expected ErrorCode corruption_detected, got %q", step.ErrorCode)
		}

		var res ScrubResult
		if err := json.Unmarshal(step.Result, &res); err != nil {
			t.Fatal(err)
		}
		if res.Corrupt != 1 {
			t.Fatalf("expected corrupt count 1, got %d", res.Corrupt)
		}
	})

	t.Run("transient S3 IO error does not flag corruption", func(t *testing.T) {
		meta := fake.NewMetadata(fake.Seed{})
		packStore := fake.NewPacks(nil)
		ref := storage.PackRef{ID: "pack-io-err", Generation: "gen-1"}
		info := createTestPack(t, packStore, ref, storage.Standard, "Warm", []byte("chunk content"))
		meta.SetPack(info)

		faultyStore := &ioErrorPackStore{PackStore: packStore}
		worker := &ScrubWorker{Store: meta, Packs: faultyStore}
		exec := worker.Executor()

		step, err := exec.Step(context.Background(), Job{}, config.GlobalSnapshot{})
		if err != nil {
			t.Fatal(err)
		}
		if step.ErrorCode != "io_error" {
			t.Fatalf("expected ErrorCode io_error, got %q", step.ErrorCode)
		}

		var res ScrubResult
		if err := json.Unmarshal(step.Result, &res); err != nil {
			t.Fatal(err)
		}
		if res.Corrupt != 0 {
			t.Fatalf("expected corrupt count 0 on transient I/O error, got %d", res.Corrupt)
		}
		if len(res.Errors) == 0 {
			t.Fatal("expected I/O error recorded in res.Errors")
		}
	})
}

type ioErrorPackStore struct {
	storage.PackStore
}

func (s *ioErrorPackStore) RangeGet(ctx context.Context, ref storage.PackRef, span storage.ByteRange) (io.ReadCloser, error) {
	return nil, fmt.Errorf("%w: simulated transient s3 timeout", storage.ErrUnavailable)
}

func TestScrubWorkerPackEligibility(t *testing.T) {
	meta := fake.NewMetadata(fake.Seed{})
	packStore := fake.NewPacks(nil)

	// 1. Glacier IR in Cold state: READABLE directly
	refIR := storage.PackRef{ID: "pack-glacier-ir", Generation: "gen-1"}
	infoIR := createTestPack(t, packStore, refIR, storage.GlacierIR, "Cold", []byte("glacier instant retrieval payload"))
	meta.SetPack(infoIR)

	// 2. Restored Glacier archive in Restored state: READABLE directly
	refRestored := storage.PackRef{ID: "pack-restored-glacier", Generation: "gen-1"}
	infoRestored := createTestPack(t, packStore, refRestored, storage.Glacier, "Restored", []byte("restored glacier payload"))
	meta.SetPack(infoRestored)

	// 3. Unrestored Glacier in Cold state: UNREADABLE without restore -> MUST be skipped
	refUnrestored := storage.PackRef{ID: "pack-unrestored-glacier", Generation: "gen-1"}
	infoUnrestored := createTestPack(t, packStore, refUnrestored, storage.Glacier, "Cold", []byte("unrestored cold payload"))
	meta.SetPack(infoUnrestored)

	worker := &ScrubWorker{Store: meta, Packs: packStore}
	exec := worker.Executor()

	step, err := exec.Step(context.Background(), Job{}, config.GlobalSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if step.ErrorCode != "" {
		t.Fatalf("expected no error code, got %q", step.ErrorCode)
	}

	var res ScrubResult
	if err := json.Unmarshal(step.Result, &res); err != nil {
		t.Fatal(err)
	}

	// 3 candidates total:
	// - pack-glacier-ir: Checked
	// - pack-restored-glacier: Checked
	// - pack-unrestored-glacier: Skipped (glacier_not_restored)
	if res.Candidates != 3 {
		t.Fatalf("expected 3 candidates, got %d", res.Candidates)
	}
	if res.Checked != 2 {
		t.Fatalf("expected 2 checked (IR + Restored), got %d", res.Checked)
	}
	if res.Skipped != 1 {
		t.Fatalf("expected 1 skipped (unrestored Glacier), got %d", res.Skipped)
	}
	if res.SkipReasons["glacier_not_restored"] != 1 {
		t.Fatalf("expected 1 glacier_not_restored reason, got %+v", res.SkipReasons)
	}
	if res.Corrupt != 0 {
		t.Fatalf("expected 0 corrupt, got %d", res.Corrupt)
	}
}

func TestScrubWorkerPaginationAndContinuation(t *testing.T) {
	meta := fake.NewMetadata(fake.Seed{})
	packStore := fake.NewPacks(nil)

	// Create 5 warm packs
	for i := 1; i <= 5; i++ {
		ref := storage.PackRef{ID: fmt.Sprintf("pack-warm-%02d", i), Generation: "gen-1"}
		info := createTestPack(t, packStore, ref, storage.Standard, "Warm", []byte(fmt.Sprintf("content-%d", i)))
		meta.SetPack(info)
	}

	worker := &ScrubWorker{Store: meta, Packs: packStore}
	exec := worker.Executor()

	// 1. First pass with limit = 2
	paramsJSON1, _ := json.Marshal(ScrubParams{Limit: 2})
	step1, err := exec.Step(context.Background(), Job{Params: paramsJSON1}, config.GlobalSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	var res1 ScrubResult
	if err := json.Unmarshal(step1.Result, &res1); err != nil {
		t.Fatal(err)
	}
	if res1.Checked != 2 || res1.Candidates != 2 {
		t.Fatalf("expected 2 checked in pass 1, got %+v", res1)
	}
	if !res1.Partial {
		t.Fatal("expected partial=true in pass 1")
	}
	if res1.NextAfter == nil || res1.NextState != "Warm" {
		t.Fatalf("expected continuation cursor in pass 1, got %+v", res1)
	}

	// 2. Second pass using continuation cursor
	paramsJSON2, _ := json.Marshal(ScrubParams{Limit: 2, State: res1.NextState, After: res1.NextAfter})
	step2, err := exec.Step(context.Background(), Job{Params: paramsJSON2}, config.GlobalSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	var res2 ScrubResult
	if err := json.Unmarshal(step2.Result, &res2); err != nil {
		t.Fatal(err)
	}
	if res2.Checked != 2 || res2.Candidates != 2 {
		t.Fatalf("expected 2 checked in pass 2, got %+v", res2)
	}
	if !res2.Partial {
		t.Fatal("expected partial=true in pass 2")
	}

	// 3. Third pass to read the final pack
	paramsJSON3, _ := json.Marshal(ScrubParams{Limit: 2, State: res2.NextState, After: res2.NextAfter})
	step3, err := exec.Step(context.Background(), Job{Params: paramsJSON3}, config.GlobalSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	var res3 ScrubResult
	if err := json.Unmarshal(step3.Result, &res3); err != nil {
		t.Fatal(err)
	}
	if res3.Checked != 1 || res3.Candidates != 1 {
		t.Fatalf("expected 1 checked in final pass, got %+v", res3)
	}
	if res3.Partial {
		t.Fatal("expected partial=false when all packs exhausted")
	}
}
