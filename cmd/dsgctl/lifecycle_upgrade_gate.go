package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"strings"

	ddbstore "github.com/KinasTomes/miles/internal/metadata/dynamodb"
)

type lifecycleUpgradeInspector interface {
	InspectLifecycleUpgrade(context.Context) ([]ddbstore.LifecycleUpgradeFinding, error)
}

// lifecycleUpgradeGate is intentionally read-only. The operator must stop all
// gateways and lifecycle workers before running it and keep them stopped until
// V2 deployment is complete; DynamoDB Scan is not a table-wide snapshot.
func lifecycleUpgradeGate(ctx context.Context, args []string, stdout io.Writer) error {
	f := newFlags("lifecycle-upgrade-gate")
	prefix := f.String("ddb-table-prefix", "", "DynamoDB table name prefix")
	region := f.String("region", "", "DynamoDB region")
	endpoint := f.String("ddb-endpoint", "", "loopback HTTP endpoint for DynamoDB Local")
	if _, ok := parse(f, args); !ok || !tablePrefix.MatchString(*prefix) || *region == "" || strings.ContainsAny(*region, "/ \t\r\n") {
		return errors.New("lifecycle-upgrade-gate needs --ddb-table-prefix <prefix> --region <region> [--ddb-endpoint <loopback-url>]")
	}
	if *endpoint != "" {
		u, e := url.Parse(*endpoint)
		if e != nil || u.Scheme != "http" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("--ddb-endpoint must be a loopback HTTP origin")
		}
		a, e := netip.ParseAddrPort(u.Host)
		if e != nil || !a.Addr().Unmap().IsLoopback() {
			return errors.New("--ddb-endpoint must be a loopback HTTP origin")
		}
	}
	o := bootstrapOptions{prefix: *prefix, region: *region, endpoint: *endpoint}
	store, closeFn, err := openBootstrapStore(o)
	if err != nil {
		return err
	}
	defer closeFn()
	inspector, ok := store.(lifecycleUpgradeInspector)
	if !ok {
		return errors.New("configured metadata store does not support lifecycle upgrade inspection")
	}
	findings, err := inspector.InspectLifecycleUpgrade(ctx)
	if err != nil {
		return fmt.Errorf("lifecycle upgrade inspection failed closed: %w", err)
	}
	blocked := false
	for _, f := range findings {
		blocked = blocked || f.Blocked
	}
	result := struct {
		ReadOnly bool                               `json:"read_only"`
		Gate     string                             `json:"gate"`
		Actions  int                                `json:"legacy_v1_actions"`
		Blocked  int                                `json:"blocked_actions"`
		Findings []ddbstore.LifecycleUpgradeFinding `json:"findings"`
	}{ReadOnly: true, Gate: "pass", Actions: len(findings), Findings: findings}
	for _, f := range findings {
		if f.Blocked {
			result.Blocked++
		}
	}
	if result.Blocked > 0 {
		result.Gate = "block"
	}
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintln(stdout, string(b)); err != nil {
		return err
	}
	if blocked {
		return errors.New("legacy lifecycle actions are unresolved; follow the owner-controlled procedure in docs/lifecycle-upgrade.md")
	}
	return nil
}
