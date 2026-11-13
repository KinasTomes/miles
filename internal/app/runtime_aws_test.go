package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
)

func TestRuntimeProviderRotatesAndFailsClosed(t *testing.T) {
	var calls atomic.Int64
	p := temporaryCredentials{aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		n := calls.Add(1)
		if n > 2 {
			return aws.Credentials{}, errors.New("helper revoked")
		}
		return aws.Credentials{AccessKeyID: "temporary", SecretAccessKey: "secret", SessionToken: "token", CanExpire: true, Expires: time.Now().Add(time.Hour)}, nil
	})}
	cache := aws.NewCredentialsCache(p)
	ctx := context.Background()
	if _, e := cache.Retrieve(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e := cache.Retrieve(ctx); e != nil || calls.Load() != 1 {
		t.Fatal(e, calls.Load())
	}
	cache.Invalidate()
	if _, e := cache.Retrieve(ctx); e != nil || calls.Load() != 2 {
		t.Fatal(e, calls.Load())
	}
	cache.Invalidate()
	if _, e := cache.Retrieve(ctx); e == nil {
		t.Fatal("revoked provider fell back")
	}
	for _, cred := range []aws.Credentials{{AccessKeyID: "static", SecretAccessKey: "secret"}, {AccessKeyID: "k", SecretAccessKey: "s", SessionToken: "t", CanExpire: true, Expires: time.Now().Add(-time.Second)}} {
		if _, e := (temporaryCredentials{aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return cred, nil })}).Retrieve(ctx); e == nil {
			t.Fatal("accepted static/expired credentials")
		}
	}
}

func TestEdgeCredentialProcessHasNoFallback(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "must-not-be-used")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "must-not-be-used")
	path := filepath.Join(t.TempDir(), "aws-config")
	for _, content := range []string{"[profile miles-runtime]\ncredential_process = /bin/false\n", "[profile miles-runtime]\naws_access_key_id = bad\naws_secret_access_key = bad\n", "[profile other]\ncredential_process = /bin/true\n"} {
		if e := os.WriteFile(path, []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
		p, e := runtimeCredentials(context.Background(), config.Startup{Profile: "edge", AWSConfigFile: path}, nil)
		if e == nil {
			_, e = p.Retrieve(context.Background())
		}
		if e == nil {
			t.Fatal("helper failure fell back to environment")
		}
	}
}

func TestEdgeUsesConfiguredAWSProfile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "aws-config")
	helper := filepath.Join(directory, "helper.sh")
	marker := filepath.Join(directory, "selected")
	if err := os.WriteFile(helper, []byte("printf selected > "+strconv.Quote(marker)+"\nexit 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	content := "[profile miles-runtime]\ncredential_process = /bin/true\n[profile member-runtime]\ncredential_process = /bin/sh " + strconv.Quote(helper) + "\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := runtimeCredentials(context.Background(), config.Startup{Profile: "edge", AWSConfigFile: path, AWSProfile: "member-runtime"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Retrieve(context.Background()); err == nil {
		t.Fatal("failed helper was accepted")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "selected" {
		t.Fatalf("configured credential process was not selected: %q %v", got, err)
	}
	for _, content := range []string{
		"[profile miles-runtime]\ncredential_process = /bin/false\n",
		"[profile member-runtime]\ncredential_process = /bin/false\naws_access_key_id = bad\naws_secret_access_key = bad\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := runtimeCredentials(context.Background(), config.Startup{Profile: "edge", AWSConfigFile: path, AWSProfile: "member-runtime"}, nil); err == nil {
			t.Fatal("accepted missing/mixed provider profile")
		}
	}
}
