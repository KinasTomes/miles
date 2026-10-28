package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/KinasTomes/miles/internal/cache"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/secretfile"
	"github.com/KinasTomes/miles/internal/storage"
	packs3 "github.com/KinasTomes/miles/internal/storage/s3"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

// Local/benchmark modes are single-node development installations. Local uses
// dummy loopback credentials; benchmark requires explicit temporary AWS credentials.
func bootstrapLocal(c config.Startup) (d Dependencies, err error) {
	if err = os.MkdirAll(c.LocalDataPath, 0700); err != nil {
		return d, err
	}
	info, err := os.Lstat(c.LocalDataPath)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return d, errors.New("local_data_path must be a non-symlink directory with permission 0700")
	}
	m, err := badger.Open(badger.Options{Path: filepath.Join(c.LocalDataPath, "metadata"), RequireProducers: true})
	if err != nil {
		return d, err
	}
	defer func() {
		if err != nil {
			_ = m.Close()
		}
	}()
	seeded, err := m.Bootstrapped(context.Background())
	if err != nil {
		return d, err
	}
	cred, err := localCredential(filepath.Join(c.LocalDataPath, "credentials.json"), !seeded)
	if err != nil {
		return d, err
	}
	// One-time authoritative dev provisioning; Bootstrap refuses to overwrite existing state.
	err = m.Bootstrap(context.Background(), badger.Bootstrap{
		Users:       []storage.User{{ID: DevUser, Active: true, Revision: 1, DefaultDomain: "default"}},
		Credentials: []storage.Credential{{ID: cred.AccessKeyID, UserID: DevUser, Active: true, Revision: 1}},
	})
	if err != nil && !errors.Is(err, storage.ErrConflict) {
		return d, err
	}
	stored, err := m.GetCredential(context.Background(), cred.AccessKeyID)
	if err != nil || stored.UserID != DevUser || stored.SystemAdmin {
		return d, errors.New("local credential does not match installation; refusing to reseed metadata")
	}
	// No default credential chain, environment credentials, IMDS, proxy or redirects.
	// Benchmark mode is separate opt-in HTTPS access with explicit temporary credentials.
	backendCred := aws.Credentials{AccessKeyID: "mileslocal", SecretAccessKey: "mileslocal"}
	if c.Mode == "benchmark" {
		backendCred, err = benchmarkCredential(c.PackCredentialsFile)
		if err != nil {
			return d, err
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	client := awss3.New(awss3.Options{Region: c.Region, BaseEndpoint: aws.String(c.PackEndpoint), UsePathStyle: true,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return backendCred, nil
		}),
		HTTPClient: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, RetryMaxAttempts: 1,
	})
	packs, err := packs3.New(client, c.PackBucket, c.PackKMSKeyID)
	if err != nil {
		transport.CloseIdleConnections()
		return d, err
	}
	lru, err := cache.NewLRU(cache.Limits{CapacityBytes: 64 << 20, MaxEntries: 1024, MaxCopyBytes: 4 << 20})
	if err != nil {
		transport.CloseIdleConnections()
		return d, err
	}
	var resident storage.Cache = lru
	closeCache := func() error { return nil }
	cachePolicy := config.DefaultCachePolicy()
	if c.Mode == "benchmark" {
		// Preserve the measured memory-only profile across local cache upgrades.
		cachePolicy.AdmitOnWrite = false
	} else {
		disk, err := cache.NewDisk(filepath.Join(c.LocalDataPath, "cache"), cache.Limits{CapacityBytes: 1 << 30, MaxEntries: 16384, MaxCopyBytes: 4 << 20})
		if err != nil {
			transport.CloseIdleConnections()
			return d, err
		}
		node, err := cache.NewNode("local", lru, disk)
		if err != nil {
			_ = disk.Close()
			transport.CloseIdleConnections()
			return d, err
		}
		resident, closeCache = node, node.Close
	}
	objects, metadata, stopObjects, err := localObjects(m, packs, resident, cachePolicy)
	if err != nil {
		_ = closeCache()
		transport.CloseIdleConnections()
		return d, err
	}
	probe := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if _, err := m.GetCredential(ctx, cred.AccessKeyID); err != nil {
			return err
		}
		if _, err := client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String(c.PackBucket)}); err != nil {
			return errors.New("local pack backend unavailable")
		}
		return ctx.Err()
	}
	return Dependencies{Metadata: metadata, Packs: coordination.ProtectPackStore(packs), Cache: resident, Objects: objects, Probe: probe, DevCredentials: []DevCredential{cred}, Close: func() error {
		stopObjects()
		transport.CloseIdleConnections()
		return errors.Join(closeCache(), m.Close())
	}}, nil
}

func benchmarkCredential(path string) (aws.Credentials, error) {
	b, err := secretfile.Read(path, 16<<10)
	if err != nil {
		return aws.Credentials{}, err
	}
	var c struct {
		AccessKey string `json:"access_key_id"`
		Secret    string `json:"secret_access_key"`
		Token     string `json:"session_token"`
	}
	if json.Unmarshal(b, &c) != nil || c.AccessKey == "" || c.Secret == "" || c.Token == "" {
		return aws.Credentials{}, errors.New("benchmark requires explicit temporary credentials with session_token")
	}
	return aws.Credentials{AccessKeyID: c.AccessKey, SecretAccessKey: c.Secret, SessionToken: c.Token}, nil
}

func localCredential(path string, allowCreate bool) (DevCredential, error) {
	b, err := secretfile.Read(path, 4096)
	if err != nil {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			return DevCredential{}, err
		}
		if !allowCreate {
			return DevCredential{}, errors.New("local credential file missing; restore original credentials.json; existing metadata is preserved")
		}
		cred, err := newDevCredential(rand.Reader, DevUser)
		if err != nil {
			return DevCredential{}, err
		}
		b, err = json.Marshal(cred)
		if err != nil {
			return DevCredential{}, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return DevCredential{}, errors.New("cannot create local credential file")
		}
		_, writeErr := f.Write(append(b, '\n'))
		syncErr := f.Sync()
		closeErr := f.Close()
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			return DevCredential{}, errors.New("cannot persist local credential file")
		}
		directory, err := os.Open(filepath.Dir(path))
		if err != nil {
			return DevCredential{}, errors.New("cannot open credential directory")
		}
		syncErr = directory.Sync()
		closeErr = directory.Close()
		if errors.Join(syncErr, closeErr) != nil {
			return DevCredential{}, errors.New("cannot sync credential directory")
		}
		return cred, nil
	}
	var cred DevCredential
	if json.Unmarshal(b, &cred) != nil || cred.UserID != DevUser || len(cred.AccessKeyID) != 20 {
		return cred, errors.New("invalid local credential file")
	}
	for _, r := range cred.AccessKeyID {
		if !((r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return cred, errors.New("invalid local access key")
		}
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(cred.Secret)
	if err != nil || len(raw) != 30 {
		return cred, errors.New("invalid local secret")
	}
	return cred, nil
}
