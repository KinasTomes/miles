package format

import (
	"bytes"
	"testing"
)

func TestUnicodeCanonicalSubset(t *testing.T) {
	got, err := Encode(map[string]any{"\ue000": 1, "𐀀": 2, "key": "<>&\u2028\n"})
	want := []byte("{\"key\":\"<>&\u2028\\n\",\"𐀀\":2,\"\ue000\":1}")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("canonical UTF16/escaping: %s %v", got, err)
	}
	if _, err = Encode(string([]byte{255})); err == nil {
		t.Fatal("invalid UTF8 replaced silently")
	}
	x := map[string]any{}
	x["cycle"] = x
	if _, err = Encode(x); err == nil {
		t.Fatal("cycle accepted")
	}
}
