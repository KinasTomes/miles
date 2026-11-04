package app

import (
	"context"
	"errors"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	"github.com/KinasTomes/miles/internal/metadata/operations"
	"github.com/KinasTomes/miles/internal/recovery"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// ProvisionShared is offline first-install provisioning, under an operator
// provider with no running nodes. It refuses every nonempty table. An uncertain
// failure needs reconciliation with the retained token; never retry as a reset.
func ProvisionShared(ctx context.Context, c config.Startup, b storage.AdminBootstrap) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Mode != "runtime" || c.RuntimeNodeID == "" {
		return storage.ErrInvalid
	}
	clients, err := newRuntimeClients(ctx, c)
	if err != nil {
		return err
	}
	defer clients.transport.CloseIdleConnections()
	id, err := storage.NewBucketID()
	if err != nil {
		return err
	}
	utc := coordination.Chrony{Clock: clock.NewReal(), Reference: c.UTCReference, RestartID: id}
	if _, err = utc.Bounds(ctx); err != nil {
		return err
	}
	// Validate the installation-wide secret without generating or rotating keys.
	if _, _, _, err = clients.keys(ctx, c.RuntimeSecretARN); err != nil {
		return err
	}
	m, err := sharedStore(c, clients, utc)
	if err != nil {
		return err
	}
	for name := range dynamodb.Tables() {
		page, e := clients.metadata.Scan(ctx, &ddb.ScanInput{TableName: aws.String(c.DDBTablePrefix + name), ConsistentRead: aws.Bool(true), Limit: aws.Int32(1)})
		if e != nil {
			return errors.New("cannot verify empty shared metadata")
		}
		if page == nil || len(page.Items) != 0 || len(page.LastEvaluatedKey) != 0 {
			return errors.New("shared installation is not empty; refusing provisioning")
		}
	}
	if err = m.BootstrapAdmin(ctx, b); err != nil {
		return err
	}
	return provisionSharedHistory(ctx, m, clients, c, b, id)
}

func provisionSharedHistory(ctx context.Context, m *dynamodb.Store, clients runtimeClients, c config.Startup, b storage.AdminBootstrap, incarnation string) error {
	history, err := storage.NewBucketID()
	if err != nil {
		return err
	}
	if err = m.InitializeR1(ctx, incarnation, history, 0); err != nil {
		return err
	}
	user, err := m.GetUser(ctx, b.Token.UserID)
	if err != nil {
		return err
	}
	token, err := m.GetAdminToken(ctx, b.Token.ID)
	if err != nil {
		return err
	}
	quota, err := m.GetQuota(ctx, b.Token.UserID)
	if err != nil {
		return err
	}
	policy, err := m.GetPolicy(ctx, 0)
	if err != nil {
		return err
	}
	// Genesis records the exact offline installation base, including hashed
	// bearer state and policy. No plaintext credential/token is written to S3.
	rows := []recovery.Effect{}
	add := func(entity, id string, revision uint64, v any) error {
		row, e := operations.Projection(entity, id, revision, v, false)
		rows = append(rows, row)
		return e
	}
	if err = add("Users", user.ID, user.Revision, user); err != nil {
		return err
	}
	if err = add("AdminTokens", token.ID, token.Revision, token); err != nil {
		return err
	}
	if err = add("UsersQuota", user.ID, user.Revision, struct {
		Owner string
		Limit int64
	}{user.ID, quota.Limit}); err != nil {
		return err
	}
	if err = add("SystemConfig", "global-policy", policy.Revision, policy); err != nil {
		return err
	}
	objects, err := recovery.NewS3Objects(clients.packs, c.PackBucket, "", c.PackKMSKeyID)
	if err != nil {
		return err
	}
	_, err = recovery.ProvisionAuditedHistory(ctx, m, objects, history, incarnation, rows, nil)
	return err
}
