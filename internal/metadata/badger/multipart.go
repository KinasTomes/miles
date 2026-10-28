package badger

import (
	"context"
	s "github.com/KinasTomes/miles/internal/storage"
)

var _ s.MultipartStore = (*Store)(nil)
var _ s.PublisherProtection = (*Store)(nil)

func (m *Store) CreateUpload(c context.Context, r s.CreateUploadRequest) (s.MultipartUpload, error) {
	return m.multipartOps().Create(c, r)
}
func (m *Store) GetUpload(c context.Context, id string) (s.MultipartUpload, error) {
	return m.multipartOps().Get(c, id)
}
func (m *Store) RegisterPartPublisher(c context.Context, r s.MultipartRequest, p s.PartPublisher) (s.PartPublisher, error) {
	return m.multipartOps().Register(c, r, p)
}
func (m *Store) PublishPart(c context.Context, r s.PartPublishRequest) (s.UploadPart, error) {
	return m.multipartOps().Publish(c, r)
}
func (m *Store) CancelPartPublisher(c context.Context, r s.MultipartRequest, id string) error {
	return m.multipartOps().Cancel(c, r, id)
}
func (m *Store) ListParts(c context.Context, r s.MultipartRequest, a uint32, n int) (s.PartListPage, error) {
	return m.multipartOps().List(c, r, a, n)
}
func (m *Store) FreezeCompletion(c context.Context, r s.FreezeRequest) (s.MultipartUpload, error) {
	return m.multipartOps().Freeze(c, r)
}
func (m *Store) PrepareCompletion(c context.Context, r s.PrepareCompletionRequest) (s.MultipartUpload, error) {
	return m.multipartOps().Prepare(c, r)
}
func (m *Store) ClaimCompletion(c context.Context, r s.MultipartRequest) (s.MultipartUpload, error) {
	return m.multipartOps().Lease(c, r, true)
}
func (m *Store) RenewCompletion(c context.Context, r s.MultipartRequest) (s.MultipartUpload, error) {
	return m.multipartOps().Lease(c, r, false)
}
func (m *Store) ReopenUpload(c context.Context, r s.MultipartRequest) (s.MultipartUpload, error) {
	return m.multipartOps().Terminal(c, r, true)
}
func (m *Store) AbortUpload(c context.Context, r s.MultipartRequest) (s.MultipartUpload, error) {
	return m.multipartOps().Terminal(c, r, false)
}
func (m *Store) GetMultipartReceipt(c context.Context, r s.MultipartRequest, d string) (s.CompletionReceipt, error) {
	return m.multipartOps().Receipt(c, r, d)
}
func (m *Store) ListPublishers(c context.Context, id, a string, n int) (s.PublisherPage, error) {
	return (multipartBridge{m}).Publishers(c, id, a, n)
}
func (m *Store) RetainPublisherPacks(c context.Context, r s.MultipartRequest, id string, p []s.PackRef) error {
	return m.multipartOps().Retain(c, r, id, p)
}
