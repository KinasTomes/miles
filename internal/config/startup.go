package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Startup is process configuration, separate from revisioned global policy.
type Startup struct {
	Profile             string
	Mode                string
	HealthAddress       string
	ShutdownSeconds     int
	S3Address           string
	AdminAddress        string
	Region              string
	LocalDataPath       string
	PackEndpoint        string
	PackBucket          string
	PackCredentialsFile string
	PackKMSKeyID        string
	RuntimeNodeID       string
	DDBTablePrefix      string
	RuntimeSecretARN    string
	AWSConfigFile       string
	AWSProfile          string
	UTCReference        string
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
		case "admin_address":
			target = &c.AdminAddress
		case "region":
			target = &c.Region
		case "local_data_path":
			target = &c.LocalDataPath
		case "pack_endpoint":
			target = &c.PackEndpoint
		case "pack_bucket":
			target = &c.PackBucket
		case "pack_credentials_file":
			target = &c.PackCredentialsFile
		case "pack_kms_key_id":
			target = &c.PackKMSKeyID
		case "shutdown_seconds":
			target = &c.ShutdownSeconds
		case "runtime_node_id":
			target = &c.RuntimeNodeID
		case "ddb_table_prefix":
			target = &c.DDBTablePrefix
		case "runtime_secret_arn":
			target = &c.RuntimeSecretARN
		case "aws_config_file":
			target = &c.AWSConfigFile
		case "aws_profile":
			target = &c.AWSProfile
		case "utc_reference":
			target = &c.UTCReference
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
	if c.AWSProfile != "" && (c.Profile != "edge" || c.Mode != "runtime" || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`).MatchString(c.AWSProfile)) {
		return fmt.Errorf("aws_profile requires an explicit Edge runtime profile name")
	}
	if c.Profile != "cloud" && c.Profile != "edge" {
		return fmt.Errorf("profile must be cloud or edge")
	}
	if c.Mode != "dev" && c.Mode != "local" && c.Mode != "benchmark" && c.Mode != "runtime" {
		return fmt.Errorf("mode must be dev, local, benchmark or runtime")
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
		return fmt.Errorf("%s mode must bind loopback", c.Mode)
	}
	if c.S3Address != "" {
		if c.Mode == "runtime" && c.RuntimeNodeID == "" {
			return fmt.Errorf("s3_address requires mode dev, local or benchmark")
		}
		a, err := netip.ParseAddrPort(c.S3Address)
		if err != nil || !(a.Addr().Unmap().IsLoopback() || c.Mode == "runtime" && c.Profile == "cloud" && a.Addr().Unmap().IsPrivate()) {
			return fmt.Errorf("MVP s3_address must bind literal loopback")
		}
	}
	if c.AdminAddress != "" {
		// Admin needs an IdentityStore; only the Badger-backed modes provide one.
		if c.Mode != "local" && c.Mode != "benchmark" && !(c.Mode == "runtime" && c.RuntimeNodeID != "") {
			return fmt.Errorf("admin_address requires mode local or benchmark")
		}
		a, err := netip.ParseAddrPort(c.AdminAddress)
		if err != nil || !(a.Addr().Unmap().IsLoopback() || c.Mode == "runtime" && c.Profile == "cloud" && a.Addr().Unmap().IsPrivate()) {
			return fmt.Errorf("MVP admin_address must bind literal loopback")
		}
	}
	if c.Region == "" || strings.ContainsAny(c.Region, "/ \t\r\n") {
		return fmt.Errorf("invalid region")
	}
	if c.Mode == "local" || c.Mode == "benchmark" {
		if c.LocalDataPath == "" || c.PackBucket == "" {
			return fmt.Errorf("%s mode requires local_data_path and pack_bucket", c.Mode)
		}
		u, err := url.Parse(c.PackEndpoint)
		if err != nil || u.User != nil || u.Host == "" || u.RawQuery != "" || u.ForceQuery || u.RawPath != "" || strings.Contains(c.PackEndpoint, "#") || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return fmt.Errorf("pack_endpoint must be an explicit origin without credentials, path or query")
		}
		if c.Mode == "local" {
			a, err := netip.ParseAddrPort(u.Host)
			if u.Scheme != "http" || err != nil || !a.Addr().Unmap().IsLoopback() || a.Port() == 0 || c.PackCredentialsFile != "" || c.PackKMSKeyID != "" {
				return fmt.Errorf("local pack_endpoint must be HTTP loopback with explicit port and dummy credentials")
			}
		} else if u.Scheme != "https" || c.PackCredentialsFile == "" || c.PackKMSKeyID == "" {
			return fmt.Errorf("benchmark mode requires HTTPS pack_endpoint, pack_credentials_file and pack_kms_key_id")
		}
	} else if c.Mode == "runtime" && c.RuntimeNodeID != "" {
		if err := c.validateShared(); err != nil {
			return err
		}
	} else if c.LocalDataPath != "" || c.PackEndpoint != "" || c.PackBucket != "" || c.PackCredentialsFile != "" || c.PackKMSKeyID != "" {
		return fmt.Errorf("storage configuration requires mode local or benchmark")
	}
	if c.RuntimeNodeID == "" || c.Mode != "runtime" {
		if c.DDBTablePrefix != "" || c.RuntimeSecretARN != "" || c.AWSConfigFile != "" || c.UTCReference != "" || c.RuntimeNodeID != "" {
			return fmt.Errorf("shared configuration requires runtime mode and runtime_node_id")
		}
	}
	if c.ShutdownSeconds < 1 || c.ShutdownSeconds > 1800 {
		return fmt.Errorf("shutdown_seconds must be 1..1800")
	}
	return nil
}

func (c Startup) validateShared() error {
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,48}$`).MatchString(c.RuntimeNodeID) || !regexp.MustCompile(`^[A-Za-z0-9_.-]{1,200}$`).MatchString(c.DDBTablePrefix) {
		return fmt.Errorf("invalid runtime_node_id or ddb_table_prefix")
	}
	ref, err := netip.ParseAddr(c.UTCReference)
	if err != nil || !ref.Is4() || ref.IsUnspecified() || ref.IsLoopback() {
		return fmt.Errorf("utc_reference must identify the approved IPv4 chrony source")
	}
	if c.S3Address == "" || c.AdminAddress == "" || !filepath.IsAbs(c.LocalDataPath) || c.PackBucket == "" || c.PackKMSKeyID == "" || c.PackEndpoint != "" || c.PackCredentialsFile != "" {
		return fmt.Errorf("shared runtime requires listeners, absolute local_data_path, pack_bucket and pack_kms_key_id; endpoint/static credential overrides are forbidden")
	}
	for _, s := range []struct {
		value string
		port  uint16
	}{{c.S3Address, 8080}, {c.AdminAddress, 9000}, {c.HealthAddress, 9100}} {
		a, err := netip.ParseAddrPort(s.value)
		if err != nil || a.Port() != s.port || c.Profile == "edge" && !a.Addr().Unmap().IsLoopback() {
			return fmt.Errorf("runtime listeners require S3 8080, Admin 9000, health 9100; Edge binds loopback")
		}
	}
	if !strings.HasPrefix(c.RuntimeSecretARN, "arn:aws:secretsmanager:"+c.Region+":") || strings.ContainsAny(c.RuntimeSecretARN, " \t\r\n") {
		return fmt.Errorf("runtime_secret_arn must name a regional Secrets Manager secret")
	}
	if c.Profile == "edge" && !filepath.IsAbs(c.AWSConfigFile) || c.Profile == "cloud" && c.AWSConfigFile != "" {
		return fmt.Errorf("Edge requires aws_config_file; Cloud uses only its instance profile")
	}
	return nil
}
func (c Startup) ShutdownTimeout() time.Duration {
	return time.Duration(c.ShutdownSeconds) * time.Second
}
