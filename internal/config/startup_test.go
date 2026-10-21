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
