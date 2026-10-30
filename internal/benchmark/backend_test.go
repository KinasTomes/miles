package benchmark

import (
	"context"
	"testing"
)

func TestWarmHarnessRetainsDeclaredBackendAndNoFinalGate(t *testing.T) {
	for _, backend := range []string{"badger", "dynamodb-local", "dynamodb"} {
		t.Run(backend, func(t *testing.T) {
			c, o, _ := setup(t)
			c.Target = "miles-warm"
			c.Environment.Metadata, c.Environment.GatewayCommit, c.Environment.PolicyRevision = backend, "test-source", "1"
			r, err := Run(context.Background(), c, o)
			if err != nil || r.Status != "passed" || r.DeclaredEnvironment.Metadata != backend || r.MVPThresholdApplied || len(r.Limitations) == 0 {
				t.Fatal(r, err)
			}
		})
	}
	c, _, _ := setup(t)
	c.Target = "miles-warm"
	c.Environment.Metadata, c.Environment.GatewayCommit, c.Environment.PolicyRevision = "ram", "test-source", "1"
	if _, _, err := newClient(c, 1); err == nil {
		t.Fatal("unknown Miles backend accepted")
	}
}
