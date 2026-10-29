package edge_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	api "github.com/KinasTomes/miles/internal/api/s3"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const proxyImage = "nginx:1.30.6-alpine@sha256:0e52bd054126cdc040b1276c0af347aeb02a41b9cfe305d6094767ac30e97949"
const accessKey = "ABCDEFGHIJKLMNOPQRST"
const secret = "edge-proxy-fixture-only"

type identity struct{}

func (identity) Credential(_ context.Context, id string) (api.Credential, error) {
	if id != accessKey {
		return api.Credential{}, storage.ErrNotFound
	}
	return api.Credential{UserID: "fixture-owner", Secret: secret, Active: true}, nil
}
func (identity) Authorize(_ context.Context, id, bucket string) (storage.Authorization, error) {
	if id != accessKey || bucket != "proxy-bucket" {
		return storage.Authorization{}, storage.ErrAccessDenied
	}
	return storage.Authorization{UserID: "fixture-owner", CredentialID: id, Read: true, Write: true,
		Bucket: storage.Bucket{Name: bucket, ID: "fixture-bucket", OwnerID: "fixture-owner", Domain: "fixture", Active: true, Revision: 1}}, nil
}

type observed struct {
	Host, URI, Metadata, SHA256 string
}

// Opt-in: real nginx/TLS and the native SigV4 router, no AWS or storage traffic.
func TestEdgeTLSProxyPreservesSignedRequests(t *testing.T) {
	if os.Getenv("MILES_EDGE_PROXY_TEST") != "1" || runtime.GOOS != "linux" {
		t.Skip("set MILES_EDGE_PROXY_TEST=1 on Linux with Docker for the real TLS proxy fixture")
	}
	now := time.Now().UTC()
	echo := func(w http.ResponseWriter, request *api.Request) error {
		if request.Key == "slow-response" {
			time.Sleep(65 * time.Second)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		w.Header().Set("Content-Type", "application/json")
		return json.NewEncoder(w).Encode(observed{Host: request.HTTP.Host, URI: request.HTTP.RequestURI,
			Metadata: request.HTTP.Header.Get("X-Amz-Meta-With_Underscore"), SHA256: hex.EncodeToString(sum[:])})
	}
	router := &api.Router{Region: "ap-southeast-1", Credentials: identity{}, Access: identity{},
		Now: func() time.Time { return now }, Handlers: map[api.Operation]api.Handler{
			api.GetObject: echo, api.PutObject: echo, api.ListObjectsV2: echo}}
	backend := httptest.NewServer(router)
	t.Cleanup(backend.Close)
	directory := t.TempDir()
	certificate, err := proxyCertificate(directory)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	config, err := os.ReadFile("nginx.conf")
	if err != nil {
		t.Fatal(err)
	}
	server := strings.ReplaceAll(string(config), "listen 443 ssl;", "listen "+address+" ssl;")
	server = strings.ReplaceAll(server, "http://127.0.0.1:8080", backend.URL)
	complete := "events {}\nhttp {\naccess_log off;\nerror_log stderr;\n" + server + "\n}\n"
	configPath := filepath.Join(directory, "nginx.conf")
	if err := os.WriteFile(configPath, []byte(complete), 0600); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("miles-edge-proxy-%d", time.Now().UnixNano())
	docker := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	}
	t.Cleanup(func() {
		if output, err := docker("rm", "-f", name); err != nil {
			t.Logf("fixture removal: %v: %s", err, output)
		}
	})
	args := []string{"run", "-d", "--rm", "--name", name, "--network", "host", "--read-only",
		"--tmpfs", "/var/cache/nginx", "--tmpfs", "/var/run",
		"--mount", "type=bind,src=" + configPath + ",dst=/etc/nginx/nginx.conf,readonly",
		"--mount", "type=bind,src=" + directory + ",dst=/etc/miles/tls,readonly",
		"--entrypoint", "nginx", proxyImage, "-g", "daemon off;"}
	if output, err := docker(args...); err != nil {
		t.Fatalf("nginx startup: %v: %s", err, output)
	}
	trust := x509.NewCertPool()
	if !trust.AppendCertsFromPEM(certificate) {
		t.Fatal("invalid fixture certificate")
	}
	tlsConfig := &tls.Config{RootCAs: trust, MinVersion: tls.VersionTLS12}
	ready := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		connection, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, tlsConfig)
		if err == nil {
			connection.Close()
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		output, _ := docker("logs", name)
		t.Fatalf("TLS proxy never became ready: %s", output)
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig, Proxy: nil, DisableCompression: true}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 80 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	sign := func(method, uri string, body []byte) *http.Request {
		request, err := http.NewRequest(method, "https://"+address+uri, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Host = "edge.example:443"
		request.Header.Set("X-Amz-Meta-With_Underscore", "signed-value")
		sum := sha256.Sum256(body)
		hash := hex.EncodeToString(sum[:])
		request.Header.Set("X-Amz-Content-Sha256", hash)
		// SigV4 excludes the scheme. Keep an explicitly signed :443 instead of
		// the SDK's default-port cleanup, then send the request over real TLS.
		originalQuery := request.URL.RawQuery
		request.URL.Scheme = "http"
		if err := v4.NewSigner().SignHTTP(context.Background(),
			aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secret},
			request, hash, "s3", "ap-southeast-1", now,
			func(options *v4.SignerOptions) { options.DisableURIPathEscaping = true }); err != nil {
			t.Fatal(err)
		}
		request.URL.Scheme = "https"
		request.URL.RawQuery = originalQuery // Canonically equivalent; test the original wire bytes.
		return request
	}
	cases := []struct {
		method, uri string
		body        []byte
	}{
		{"GET", "/proxy-bucket/a//b/../c+%2F%252F.txt", nil},
		{"GET", "/proxy-bucket/%E1%BB%A9ng%20d%E1%BB%A5ng/%25+//key", nil},
		{"GET", "/proxy-bucket?list-type=2&prefix=a%2Fb+z&max-keys=7&continuation-token=a%2Bb%2520", nil},
		{"PUT", "/proxy-bucket/signed-body", []byte("binary\x00payload\nwith bytes")},
	}
	if os.Getenv("MILES_EDGE_PROXY_SLOW_TEST") == "1" {
		cases = append(cases, struct {
			method, uri string
			body        []byte
		}{"GET", "/proxy-bucket/slow-response", nil})
	}
	for _, test := range cases {
		t.Run(test.uri, func(t *testing.T) {
			response, err := client.Do(sign(test.method, test.uri, test.body))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			raw, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("native signed request rejected: %d %s %v", response.StatusCode, raw, err)
			}
			var got observed
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(test.body)
			if got.Host != "edge.example:443" || got.URI != test.uri || got.Metadata != "signed-value" || got.SHA256 != hex.EncodeToString(sum[:]) {
				t.Fatalf("proxy altered signed request: %+v", got)
			}
		})
	}
	request := sign("GET", "/proxy-bucket/negative-control", nil)
	request.Host = "tampered.example:443"
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("altered signed Host accepted: %d", response.StatusCode)
	}
}

func proxyCertificate(directory string) ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	private, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(filepath.Join(directory, "fullchain.pem"), certificate, 0600); err != nil {
		return nil, err
	}
	err = os.WriteFile(filepath.Join(directory, "private-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private}), 0600)
	return certificate, err
}
