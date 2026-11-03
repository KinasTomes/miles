package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"

	"github.com/KinasTomes/miles/internal/jobs"
)

// The privileged lifecycle controller consumes a marker bound to this systemd
// invocation. It owns ALB/ASG calls; the gateway neither shells out nor gets IAM
// privileges for lifecycle completion. An old invocation cannot drain a new one.
func requestCloudDrain(ctx context.Context, directory string, node jobs.Node) error {
	invocation := os.Getenv("INVOCATION_ID")
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(invocation) {
		return errors.New("Cloud drain requires a bound systemd invocation")
	}
	data, e := json.Marshal(map[string]string{"invocation": invocation, "node_id": node.ID, "incarnation": node.Incarnation, "service_incarnation": node.ServiceIncarnation})
	if e != nil {
		return e
	}
	path := filepath.Join(directory, "drain-request.json")
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm() != 0600) {
		return errors.New("unsafe drain marker")
	}
	file, e := os.CreateTemp(directory, ".drain-request-*")
	if e != nil {
		return e
	}
	defer os.Remove(file.Name())
	_, e = file.Write(data)
	e = errors.Join(e, file.Sync(), file.Close())
	if e != nil {
		return e
	}
	if e = os.Rename(file.Name(), path); e != nil {
		return e
	}
	if e = syncDir(directory); e != nil {
		return e
	}
	// Wait for systemd to stop this invocation. Returning early would cause the
	// generic worker supervisor to cancel heartbeats before drain actually starts.
	<-ctx.Done()
	return ctx.Err()
}
