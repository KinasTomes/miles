package benchmarknative

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validBinding() Config {
	return Config{Account: "123456789012", RoleARN: "arn:aws:iam::123456789012:role/test/deploy",
		Region: "ap-southeast-1", Bucket: "native-physical", LogicalBucket: "logical-fixture",
		KMSKey: "arn:aws:kms:ap-southeast-1:123456789012:key/fixture", RunID: "fixture-run", Slot: "recovery-cloud-r1-rep1",
		Source: strings.Repeat("a", 40), History: strings.Repeat("b", 32), Incarnation: strings.Repeat("c", 32),
		Tags: map[string]string{"Project": "Miles", "Environment": "dev"}, StopAt: time.Now().Add(10 * time.Minute)}
}

func TestNativeBindingRejectsMissingOrCrossAccountScope(t *testing.T) {
	c := validBinding()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.LogicalBucket = "" }, func(c *Config) { c.History = "not-history" },
		func(c *Config) { c.KMSKey = "arn:aws:kms:ap-southeast-1:999999999999:key/foreign" },
		func(c *Config) { c.RoleARN = "arn:aws:iam::999999999999:role/foreign" },
	} {
		bad := validBinding()
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatal("invalid binding accepted")
		}
	}
}

func TestNativeSessionRejectsStaticAndExpiredKeysBeforeSTS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	for _, body := range []string{
		`{"Version":1,"AccessKeyId":"AKIAfixture","SecretAccessKey":"private","SessionToken":"token","Expiration":"2099-01-01T00:00:00Z"}`,
		`{"Version":1,"AccessKeyId":"ASIAfixture","SecretAccessKey":"private","SessionToken":"token","Expiration":"2000-01-01T00:00:00Z"}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Session(t.Context(), validBinding(), path); err == nil || !strings.Contains(err.Error(), "temporary AWS credentials") {
			t.Fatal(err)
		}
	}
}
