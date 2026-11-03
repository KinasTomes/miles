package dynamodb

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/KinasTomes/miles/internal/jobs"
	opstest "github.com/KinasTomes/miles/internal/jobs/contracttest"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type activeJobsScan struct {
	API
	calls int
	t     *testing.T
}

func (a *activeJobsScan) Scan(_ context.Context, in *ddb.ScanInput, _ ...func(*ddb.Options)) (*ddb.ScanOutput, error) {
	a.calls++
	if aws.ToString(in.FilterExpression) != "#state IN (:queued,:running,:cancelling)" || !aws.ToBool(in.ConsistentRead) {
		a.t.Fatal("worker scan is not a filtered consistent base-table read", in)
	}
	if a.calls == 1 {
		return &ddb.ScanOutput{LastEvaluatedKey: Item{"job_id": text("job-terminal")}}, nil
	}
	if v, ok := in.ExclusiveStartKey["job_id"]; !ok || v == nil {
		a.t.Fatal("empty filtered page lost its native continuation")
	}
	j := jobs.Job{ID: "job-queued", Type: "gc", State: jobs.Queued, Revision: 1, PolicyRevision: 1, Params: json.RawMessage(`{}`), CreatedAt: opstest.Now, Progress: map[string]uint64{}}
	item, e := nativeItem(j)
	if e != nil {
		a.t.Fatal(e)
	}
	item["job_id"] = text(j.ID)
	return &ddb.ScanOutput{Items: []Item{item}}, nil
}
func TestRunnableScanRetainsEmptyNativePages(t *testing.T) {
	api := &activeJobsScan{t: t}
	m := &Store{api: api, tables: map[string]string{"Jobs": "fixture-Jobs"}}
	p, e := m.ListJobs(context.Background(), jobs.PageRequest{Limit: 1, ActiveOnly: true})
	if e != nil || len(p.Jobs) != 0 || p.Next != "job-terminal" {
		t.Fatal(p, e)
	}
	p, e = m.ListJobs(context.Background(), jobs.PageRequest{Limit: 1, ActiveOnly: true, Cursor: p.Next})
	if e != nil || len(p.Jobs) != 1 || p.Jobs[0].ID != "job-queued" || p.Next != "" {
		t.Fatal(p, e)
	}
}
