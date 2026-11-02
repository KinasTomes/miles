package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
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
	base, err := recovery.Encode(struct {
		Version int                    `json:"runtime_genesis_version"`
		User    storage.User           `json:"user"`
		Token   storage.AdminToken     `json:"admin_token"`
		Quota   storage.LogicalQuota   `json:"quota"`
		Policy  storage.PolicySnapshot `json:"policy"`
	}{1, user, token, quota, policy})
	if err != nil {
		return err
	}
	sum := sha256.Sum256(base)
	objects, err := recovery.NewS3Objects(clients.packs, c.PackBucket, "", c.PackKMSKeyID)
	if err != nil {
		return err
	}
	catalog := recovery.Catalog{Version: 1, History: history, Incarnation: incarnation, Genesis: recovery.Reference{SHA256: hex.EncodeToString(sum[:]), Length: len(base)}}
	return recovery.ProvisionHistory(ctx, m, objects, catalog, base)
}
