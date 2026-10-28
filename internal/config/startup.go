package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Startup is process configuration, separate from revisioned global policy.
type Startup struct {
	Profile         string
	Mode            string
	HealthAddress   string
	ShutdownSeconds int
	S3Address       string
	Region          string
	LocalDataPath   string
	PackEndpoint    string
	PackBucket      string
}

func DefaultStartup() Startup {
	return Startup{Profile: "edge", Mode: "runtime", HealthAddress: "127.0.0.1:9100", ShutdownSeconds: 1800, Region: "ap-southeast-1"}
}

// LoadStartup rejects unknown/duplicate fields and multiple JSON values.
func LoadStartup(r io.Reader) (Startup, error) {
	c := DefaultStartup()
	b, err := io.ReadAll(io.LimitReader(r, 64*1024+1))
	if err != nil {
		return c, err
	}
	if len(b) > 64*1024 {
		return c, fmt.Errorf("startup config exceeds 64 KiB")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return c, fmt.Errorf("startup config must be an object")
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return c, err
		}
		name := key.(string)
		if seen[name] {
			return c, fmt.Errorf("duplicate startup field %q", name)
		}
		seen[name] = true
		var target any
		switch name {
		case "profile":
			target = &c.Profile
		case "mode":
			target = &c.Mode
		case "health_address":
			target = &c.HealthAddress
		case "s3_address":
			target = &c.S3Address
		case "region":
			target = &c.Region
		case "local_data_path":
			target = &c.LocalDataPath
		case "pack_endpoint":
			target = &c.PackEndpoint
		case "pack_bucket":
			target = &c.PackBucket
		case "shutdown_seconds":
			target = &c.ShutdownSeconds
		default:
			return c, fmt.Errorf("unknown startup field %q", name)
		}
		// Null is not an omitted field and must not silently preserve a default.
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return c, err
		}
		if bytes.Equal(raw, []byte("null")) {
			return c, fmt.Errorf("startup field %q cannot be null", name)
		}
		if err := json.Unmarshal(raw, target); err != nil {
			return c, fmt.Errorf("invalid startup field %q", name)
		}
	}
	if _, err := d.Token(); err != nil {
		return c, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("trailing startup JSON")
	}
	return c, c.Validate()
}

func (c Startup) Validate() error {
	if c.Profile != "cloud" && c.Profile != "edge" {
		return fmt.Errorf("profile must be cloud or edge")
	}
	if c.Mode != "dev" && c.Mode != "local" && c.Mode != "runtime" {
		return fmt.Errorf("mode must be dev, local or runtime")
	}
	address, err := netip.ParseAddrPort(c.HealthAddress)
	if err != nil {
		return fmt.Errorf("health_address must be a literal loopback/private IP and port")
	}
	ip := address.Addr().Unmap()
	if !ip.IsLoopback() && !ip.IsPrivate() {
		return fmt.Errorf("health_address must bind loopback/private, never wildcard/public")
	}
	if c.Mode != "runtime" && !ip.IsLoopback() {
		return fmt.Errorf("dev mode must bind loopback")
	}
	if c.S3Address != "" {
		a, err := netip.ParseAddrPort(c.S3Address)
		if err != nil || !a.Addr().Unmap().IsLoopback() {
			return fmt.Errorf("MVP s3_address must bind literal loopback")
		}
	}
	if c.Region == "" || strings.ContainsAny(c.Region, "/ \t\r\n") {
		return fmt.Errorf("invalid region")
	}
	if c.Mode == "local" {
		if c.LocalDataPath == "" || c.PackBucket == "" {
			return fmt.Errorf("local mode requires local_data_path and pack_bucket")
		}
		u, err := url.Parse(c.PackEndpoint)
		if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.RawPath != "" || strings.Contains(c.PackEndpoint, "#") || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return fmt.Errorf("pack_endpoint must be HTTP loopback with explicit port")
		}
		a, err := netip.ParseAddrPort(u.Host)
		if err != nil || !a.Addr().Unmap().IsLoopback() || a.Port() == 0 {
			return fmt.Errorf("pack_endpoint must be HTTP loopback with explicit port")
		}
	} else if c.LocalDataPath != "" || c.PackEndpoint != "" || c.PackBucket != "" {
		return fmt.Errorf("local storage configuration requires mode local")
	}
	if c.ShutdownSeconds < 1 || c.ShutdownSeconds > 1800 {
		return fmt.Errorf("shutdown_seconds must be 1..1800")
	}
	return nil
}
func (c Startup) ShutdownTimeout() time.Duration {
	return time.Duration(c.ShutdownSeconds) * time.Second
}
