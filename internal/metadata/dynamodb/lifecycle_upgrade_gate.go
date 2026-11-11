package dynamodb

import (
	"context"
	"fmt"
	"strings"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// LifecycleUpgradeFinding describes a legacy action and the durable records
// that determine whether it is safe to deploy a V2 writer.
type LifecycleUpgradeFinding struct {
	ActionID   string     `json:"action_id"`
	State      string     `json:"state"`
	JobState   jobs.State `json:"job_state,omitempty"`
	LeaseState string     `json:"lease_state,omitempty"`
	Pending    bool       `json:"r1_pending"`
	Blocked    bool       `json:"blocked"`
	Reason     string     `json:"reason,omitempty"`
}

func classifyLegacyLifecycle(a lifecycle.Action, j jobs.Job, l s.R1Lease) LifecycleUpgradeFinding {
	f := LifecycleUpgradeFinding{ActionID: a.ID, State: a.State, JobState: j.State, LeaseState: l.State}
	for _, pending := range l.Pending {
		if pending == "lifecycle/"+a.ID {
			f.Pending = true
		}
	}
	terminal := a.State == "Done" || a.State == "Skipped" || a.State == "NotApplied"
	if !terminal {
		f.Blocked, f.Reason = true, "legacy lifecycle action is nonterminal"
	}
	if !j.State.Terminal() || j.PendingStep != "" {
		f.Blocked, f.Reason = true, "associated job is not terminal or still has a pending step"
	}
	if f.Pending || l.State != "Released" || l.Resolving || l.PinCount != 0 || l.PinCursor != "" {
		f.Blocked, f.Reason = true, "associated R1 lease retains pending work, pins, or authority"
	}
	return f
}

// InspectLifecycleUpgrade is a read-only, fail-closed pre-deploy inventory.
// It fully paginates a strongly consistent scan, then strongly reads each
// action's Job and producer lease. Because DynamoDB Scan is not a table-wide
// snapshot, callers MUST first stop gateways and lifecycle workers and keep
// them stopped until deployment completes.
func (m *Store) InspectLifecycleUpgrade(ctx context.Context) ([]LifecycleUpgradeFinding, error) {
	if m == nil || m.api == nil {
		return nil, s.ErrInvalid
	}
	gateItem, err := m.getUpgradeRow(ctx, r1GateKey())
	if err != nil {
		return nil, fmt.Errorf("read R1 admission gate: %w", err)
	}
	gate, err := r1Decode[s.R1Gate](gateItem)
	// Closing is the supported admission freeze. Once Active and Resolving are
	// both zero, no lease remains that can perform lifecycle side effects, and
	// new acquisitions are rejected while the gate remains Closing.
	if err != nil || gate.State != "Closing" || gate.Active != 0 || gate.Resolving != 0 {
		return nil, fmt.Errorf("R1 gate must be Closing with zero active/resolving leases: %w", s.ErrFenced)
	}
	var items []Item
	var cursor map[string]types.AttributeValue
	for {
		out, err := m.api.Scan(ctx, &ddb.ScanInput{TableName: aws.String(m.tables["SystemConfig"]), ConsistentRead: aws.Bool(true), Limit: aws.Int32(128), ExclusiveStartKey: cursor})
		if err != nil {
			return nil, backend(err)
		}
		if out == nil {
			return nil, s.ErrUnavailable
		}
		for _, item := range out.Items {
			v, ok := item["config_id"].(*types.AttributeValueMemberS)
			if !ok || v == nil {
				return nil, s.ErrUnavailable
			}
			if strings.HasPrefix(v.Value, "lifecycle/action/") {
				items = append(items, item)
			}
		}
		cursor = out.LastEvaluatedKey
		if len(cursor) == 0 {
			break
		}
	}
	findings := make([]LifecycleUpgradeFinding, 0, len(items))
	for _, item := range items {
		keyValue := item["config_id"].(*types.AttributeValueMemberS).Value
		id := strings.TrimPrefix(keyValue, "lifecycle/action/")
		a, err := r1Decode[lifecycle.Action](item)
		if err != nil || id == "" || a.ID != id {
			return nil, fmt.Errorf("invalid lifecycle action row %q: %w", keyValue, s.ErrUnavailable)
		}
		if a.Version != lifecycle.ActionVersionLegacy {
			continue
		}
		jobKey := stringKey("Jobs", "job_id", a.JobID)
		jobItem, err := m.getUpgradeRow(ctx, jobKey)
		if err != nil {
			return nil, fmt.Errorf("read job for lifecycle action %s: %w", id, err)
		}
		j, err := r1Decode[jobs.Job](jobItem)
		if err != nil || j.ID != a.JobID || j.Type != a.Kind {
			return nil, fmt.Errorf("invalid job for lifecycle action %s: %w", id, s.ErrUnavailable)
		}
		leaseItem, err := m.getUpgradeRow(ctx, r1LeaseKey(a.JobID))
		if err != nil {
			return nil, fmt.Errorf("read R1 lease for lifecycle action %s: %w", id, err)
		}
		l, err := r1Decode[s.R1Lease](leaseItem)
		if err != nil || l.Fence.LeaseID != a.JobID || l.Fence != a.Fence {
			return nil, fmt.Errorf("invalid R1 lease for lifecycle action %s: %w", id, s.ErrUnavailable)
		}
		findings = append(findings, classifyLegacyLifecycle(a, j, l))
	}
	return findings, nil
}

func (m *Store) getUpgradeRow(ctx context.Context, k Key) (Item, error) {
	out, err := m.api.GetItem(ctx, &ddb.GetItemInput{TableName: aws.String(m.tables[k.Table]), Key: k.Attributes, ConsistentRead: aws.Bool(true)})
	if err != nil {
		return nil, backend(err)
	}
	if out == nil || len(out.Item) == 0 {
		return nil, s.ErrNotFound
	}
	return out.Item, nil
}
