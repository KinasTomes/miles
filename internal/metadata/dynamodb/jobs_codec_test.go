package dynamodb

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/KinasTomes/miles/internal/jobs"
	opstest "github.com/KinasTomes/miles/internal/jobs/contracttest"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestJobNativeRoundTrip(t *testing.T) {
	j := jobs.Job{ID: "job-fixture", Type: "gc", State: jobs.Queued, Revision: 1, PolicyRevision: 1, Params: json.RawMessage(`{}`), CreatedAt: opstest.Now, Progress: map[string]uint64{}}
	item, e := nativeItem(j)
	if e != nil {
		t.Fatal(e)
	}
	var decoded jobs.Job
	if e = unmarshalNative(&types.AttributeValueMemberM{Value: item}, reflect.ValueOf(&decoded).Elem()); e != nil {
		t.Fatal(e)
	}
	if e = decoded.Validate(); e != nil {
		t.Fatalf("invalid fixture round trip: %+v: %v", decoded, e)
	}
}
