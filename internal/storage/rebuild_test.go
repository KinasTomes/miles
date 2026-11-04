package storage

import "testing"

func TestIndexRecordIdentityFramesControlCharacters(t *testing.T) {
	a := IndexRecordKey("heads", []string{"bucket", "id\x00segment", "key"})
	b := IndexRecordKey("heads", []string{"bucket", "id", "segment\x00key"})
	if a == b {
		t.Fatal("distinct object identities collided")
	}
	if IndexRecordKey("policy-current", nil) != IndexRecordKey("policy-current", []string{}) {
		t.Fatal("empty identity changed order")
	}
}
