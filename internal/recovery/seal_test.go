package recovery

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func epochGolden(t *testing.T) (*memoryObjects, Catalog, string) {
	t.Helper()
	body, err := os.ReadFile("../../tests/fixtures/recovery/epoch-seal.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Catalog string
		Objects map[string]string
		SHA256  string `json:"seal_sha256"`
	}
	if err = json.Unmarshal(body, &fixture); err != nil {
		t.Fatal(err)
	}
	var c Catalog
	if err = Decode([]byte(fixture.Catalog), &c); err != nil {
		t.Fatal(err)
	}
	objects := &memoryObjects{objects: map[string][]byte{}}
	for key, b := range fixture.Objects {
		objects.objects[key] = []byte(b)
	}
	return objects, c, fixture.SHA256
}
func TestEpochSealProductionGoldenAndMalformedCandidates(t *testing.T) {
	ctx := context.Background()
	objects, c, sum := epochGolden(t)
	cut, err := VerifyCut(ctx, objects, c, 0)
	if err != nil || cut.Reference.SHA256 != sum {
		t.Fatal(cut, err)
	}
	for _, name := range []string{"wrong-history", "wrong-version", "chain-gap", "invalid-cut-time", "wrong-count", "wrong-digest", "excessive-tree-depth", "missing-marker", "missing-membership"} {
		t.Run(name, func(t *testing.T) {
			objects, c, _ := epochGolden(t)
			prefix := epochPrefix(c.History, 0)
			var marker Completion
			if err := Decode(objects.objects[prefix+"complete.json"], &marker); err != nil {
				t.Fatal(err)
			}
			var seal Seal
			if err := Decode(objects.objects[marker.Seal.Key], &seal); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "wrong-history":
				seal.History = c.Incarnation
			case "wrong-version":
				seal.Version = 2
			case "chain-gap":
				seal.PreviousSHA256 = c.Incarnation + c.Incarnation
			case "invalid-cut-time":
				seal.CutAt = "2026-11-01T07:00:00+07:00"
			case "wrong-count":
				seal.Events = 1
				seal.EventsSHA256 = c.History + c.History
			case "wrong-digest":
				seal.EventsSHA256 = c.History + c.History
			case "missing-marker":
				delete(objects.objects, prefix+"complete.json")
			case "missing-membership":
				delete(objects.objects, seal.Membership.Key)
			case "excessive-tree-depth":
				var n membershipNode
				if err := Decode(objects.objects[seal.Membership.Key], &n); err != nil {
					t.Fatal(err)
				}
				n.Level = maxMembershipLevel + 1
				body, err := Encode(n)
				if err != nil {
					t.Fatal(err)
				}
				key := prefix + "membership/" + digest(body) + ".json"
				objects.objects[key] = body
				seal.Membership = AttachmentReference{Key: key, Reference: ref(body)}
			}
			if name != "missing-marker" {
				body, err := Encode(seal)
				if err != nil {
					t.Fatal(err)
				}
				key := prefix + "seal-" + digest(body) + ".json"
				objects.objects[key] = body
				marker.Seal = AttachmentReference{Key: key, Reference: ref(body)}
				objects.objects[prefix+"complete.json"], err = Encode(marker)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := VerifyCut(ctx, objects, c, 0); err == nil {
				t.Fatal("invalid journal cut accepted")
			}
		})
	}
}
