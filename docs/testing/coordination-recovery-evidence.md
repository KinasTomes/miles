# Coordination và recovery prototype — evidence W2

Task: W02-PHONG-03. Owner: Phong. Baseline: `f6d9e21`.
Trạng thái: **implementation/golden candidate để review, chưa Approved/Frozen**.

Nguồn: [R1](../contracts/r1-and-cache.md) §3–8,
[R2](../contracts/r2-recovery.md) §5–12,
[Clock](../contracts/clock-and-policy.md) §3.
Không sửa shared ports/models, không thay MetadataStore hoặc tạo production recovery.

## Checklist và evidence

| Checklist Miles | Deliverable | Tests chính |
|---|---|---|
| C1 fake gate/read pins/fence/producer admission | `internal/coordination/fake/read.go`, `producer.go` | GuardRootPinAndRelease; PinRetireOrderings; CompactionAndAtomicBatch; LostAuthorityRetainsProtection; RenewDeadlineAndLostEquality; ClosingCohortAndStableOutcomes; PublicationExpiryAndFenceOrder; TakeoverRequiresClosureReconciliationAndGrace; PausedPublicationLosesToClosure |
| C2 event/descriptor/membership/seal/checkpoint goldens | `tests/fixtures/recovery/candidate.json`, `journal.json`; reference tests `internal/recovery` | JournalGoldenMultiPageChainCheckpoint; EveryMissingOrCorruptArtifact; SemanticCorruptionWithValidDigests; DuplicateConflictAndAtomicApply; HistoricalCheckpointKeepsTombstones |
| C3 failpoints | `Faults.Run`, integrated export model | RegistrationExportCommitCloseSealFaultRecovery; EveryJournalWriteFaultWindow; SealRequiresClosedExactMembershipAndDurablePages; LostCommitACKReconcilesOriginal; LostPinAndReleaseACK |

## Reference format candidate cho review

Mỗi artifact trong `journal.json` lưu exact UTF-8 body bytes dưới SHA-256 key.
Parent reference `{length,sha256}` bind byte length/digest; không self-hash.
Tất cả fields bắt buộc, kể cả zero/false/empty list; null/unknown/missing reject.
`r2_version=1` là candidate theo contract, không tự freeze shared format.

| Artifact | Required fields |
|---|---|
| Event page | effects, epoch, event, history, number, r2_version |
| Effect tham chiếu | deleted, key, revision, value |
| Event descriptor | action, entity, epoch, history, id, logical_op, mutation, pages, r2_version |
| Membership | epoch, events (sorted unique id + descriptor reference), history, r2_version |
| Seal | epoch, history, membership reference, previous seal hash/genesis anchor, r2_version |
| Checkpoint | history, projection, r2_version, terminal seal reference, sorted state, target_epoch |

Effects là **projection mẫu**, không phải schemas cho mọi authoritative entity.
Fixture có Objects object_commit, head/tombstone và quota trong cùng bundle;
không dùng scalar `value` như production object/credential/manifest schema.
History/mutation là 32 lowercase hex; event ID theo SHA-256 của canonical array
`["miles-r2-event-v1", history, mutation, entity, action]`. Logical request và
mutation identity riêng, event ID không chứa fence hoặc epoch mutable.

Giới hạn reference: body ≤256 KiB; arrays/maps ≤256; nesting ≤8; unsigned
integer ≤2^53−1, revisions positive. Chỉ nhận printable ASCII (không `<>&`) và
integer canonical subset. Không quảng bá `encoding/json` thành full RFC8785:
arbitrary Unicode/UTF-16 sorting, escaping và số thực cần serializer/conformance
riêng khi mở production. Input ngoài subset bị reject, không normalize âm thầm.
Literal body/hash được chuẩn bị độc lập bằng Python hashlib; Go đối chiếu hash,
length, parent references và state oracle lưu trong fixture. Tests không tự
regenerate fixture để làm kết quả xanh.

Hai epoch liên tiếp: epoch 0 tạo head + quota; epoch 1 tombstone + quota 0.
Mỗi event có hai pages. Historical checkpoint giữ tombstone revision 2, không
resurrect revision cũ. Replay validates mọi effects trước atomic staging apply;
duplicate same ID/digest no-op, conflicting digest reject. Revision contiguous
là rule của projection mẫu này, không áp ngầm cho mọi entity production.

## Fault timelines và LP

| Window | Before fault | After fault / lost ACK | Reconciliation |
|---|---|---|---|
| Registration | Chưa có producer | Cohort member đã tồn tại | Retry same ID/binding |
| S3 immutable write | Object chưa durable | Exact object đã tồn tại | Conditional exact-byte retry; không overwrite khác bytes |
| Commit | Mutation còn Pending | Committed outcome retained | Lookup/reconcile original receipt, không double apply |
| Epoch close | Closing chưa thành Closed | Closed tuple/membership retained | Retry FinishClose, không reopen old fences |
| Publish seal | Chưa có pointer | Pointer đã publish | Same immutable seal idempotent, mismatch reject |

`Faults.Run` hooks ngoài model lock. Tests dùng barriers/channels hoặc explicit
event order; không wall-clock sleeps. EveryJournalWriteFaultWindow kiểm cả hai
phía của từng page, descriptor, membership và seal write. Publisher lấy exact
committed membership từ `Producers.ClosedMembers`, verifies toàn payload trước LP.
Không seal bằng timeout, counter=0 hoặc một boolean "uploaded".

Read pins: guard→root handoff atomically CAS head; pin batch validates toàn batch;
pin/retire chung LP; work token giữ protection đến quiescence. `RootBound` không
giữ mọi physical generation. Cancel/Lost đóng quyền mới nhưng không tự unpin.
Producer: Closing ngăn new producer, cho admitted cohort attach tới closure;
dispatch trước lease expiry có thể commit sau expiry nhưng trước fence closure.
Takeover không cấp new authority khi Pending outcome chưa reconcile.

## Freeze/production boundaries

- Hưng/Chiến/Hải review field schema, byte goldens và integration bindings trước
  format freeze. Bản candidate này không tuyên bố freeze đã đạt.
- Fake clock là exact local time oracle; không chứng minh production UTC sample,
  provenance, skew hoặc distributed lease. Consumer dùng evaluator W02-PHONG-01.
- S3 journal sink và seal publisher trong tests là executable reference model;
  chưa có production exporter, actual checkpoint installation hoặc cloud evidence.
- Full inventory, causal dependencies giữa mọi entity, catalog/markers, tree
  paging và historical→normalized genesis security thuộc downstream format/recovery.
- Không đổi Miles status, không tuyên bố production R1/R2 hoàn tất.

## Validation commands

```text
go test -count=20 ./internal/coordination/... ./internal/recovery/...
go test ./...
go vet ./...
go test -race ./internal/coordination/... ./internal/recovery/...
```

Race chỉ PASS khi thực sự chạy thành công; lỗi GCC/cgo Windows ghi NOT VERIFIED.
File ban đầu untracked phải kiểm gofmt/whitespace trực tiếp, không chỉ git diff.
