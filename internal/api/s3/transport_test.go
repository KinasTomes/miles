package s3

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
)

// transport.json: requests of the pinned clients and their single-fault
// variants (scripts/s3capture). The router accepts each one as op, or rejects
// it with error; an error that needs the signature or body (SigV4, aws-chunked,
// checksum) is left to later stages, so Resolve still returns op. Every
// expected error is a code of API v2 2.4.
func TestTransportFixtures(t *testing.T) {
	var f struct {
		Cases []struct {
			Name    string
			Op      Operation
			Error   ErrorCode
			Request []string
			Body    string
		}
	}
	readFixture(t, "transport.json", &f)
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if _, ok := errorCodes[c.Error]; c.Error != "" && !ok {
				t.Fatalf("error %s is not in API v2 2.4", c.Error)
			}
			raw := strings.Join(c.Request, "\r\n") + "\r\n\r\n" + c.Body
			req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
			if err != nil {
				t.Fatal(err)
			}
			route, e := Resolve(req)
			switch {
			case e != nil && (c.Op != "" || e.Code != c.Error):
				t.Errorf("Resolve: %v, want op %q error %q", e, c.Op, c.Error)
			case e == nil && route.Op != c.Op:
				t.Errorf("Resolve: %s, want op %q error %q", route.Op, c.Op, c.Error)
			}
		})
	}
}
