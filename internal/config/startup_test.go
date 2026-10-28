package config

import (
	"strings"
	"testing"
)

func TestStartupBoundary(t *testing.T) {
	for _, input := range []string{`[]`, `null`, `{"mode":"dev","mode":"runtime"}`, `{"mode":null}`, `{"mode":"dev"} {}`, `{"policy":{}}`, `{"profile":"other"}`, `{"shutdown_seconds":1.5}`, `{"shutdown_seconds":0}`, `{"shutdown_seconds":1801}`, `{"health_address":"0.0.0.0:9100"}`, `{"health_address":"[::]:9100"}`, `{"health_address":"8.8.8.8:9100"}`, `{"health_address":"localhost:9100"}`, `{"mode":"dev","health_address":"10.0.0.1:9100"}`, strings.Repeat(" ", 65537)} {
		if _, err := LoadStartup(strings.NewReader(input)); err == nil {
			t.Errorf("accepted %s", input[:min(len(input), 100)])
		}
	}
	for _, input := range []string{`{}`, `{"profile":"cloud","health_address":"10.0.0.1:9100"}`, `{"health_address":"[fd00::1]:9100"}`, `{"mode":"dev","health_address":"127.0.0.1:0"}`} {
		if _, err := LoadStartup(strings.NewReader(input)); err != nil {
			t.Errorf("%s: %v", input, err)
		}
	}
	c, err := LoadStartup(strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if c != DefaultStartup() || c.Mode != "runtime" {
		t.Fatalf("unsafe defaults: %+v", c)
	}
}

func TestLocalStorageAndS3Boundary(t *testing.T) {
	valid := `{"mode":"local","s3_address":"127.0.0.1:0","local_data_path":"/tmp/miles-local","pack_endpoint":"http://127.0.0.1:12345","pack_bucket":"test"}`
	if _, err := LoadStartup(strings.NewReader(valid)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(valid, "127.0.0.1:0", "0.0.0.0:8080", 1),
		strings.Replace(valid, "http://127.0.0.1:12345", "https://s3.amazonaws.com", 1),
		strings.Replace(valid, "http://127.0.0.1:12345", "http://localhost:12345", 1),
		strings.Replace(valid, "http://127.0.0.1:12345", "http://user:secret@127.0.0.1:12345", 1),
		strings.Replace(valid, "http://127.0.0.1:12345", "http://127.0.0.1:12345/?q=x", 1),
		strings.Replace(valid, `"mode":"local"`, `"mode":"runtime"`, 1),
	} {
		if _, err := LoadStartup(strings.NewReader(bad)); err == nil {
			t.Fatal("accepted unsafe local config")
		}
	}
}

func TestDevelopmentHealthAddressErrorNamesMode(t *testing.T) {
	for _, mode := range []string{"dev", "local"} {
		c := DefaultStartup()
		c.Mode, c.HealthAddress = mode, "10.0.0.1:9100"
		if err := c.Validate(); err == nil || err.Error() != mode+" mode must bind loopback" {
			t.Fatalf("wrong mode in address error: %v", err)
		}
	}
}
