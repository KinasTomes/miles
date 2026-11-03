package live_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const slowParts = 70
const slowPartBytes = 5 << 20
const slowKey = "/slow-multipart"

type slowState struct {
	Bucket     string     `json:"bucket"`
	Credential credential `json:"credential"`
	UploadID   string     `json:"upload_id"`
	ETags      []string   `json:"etags"`
	SHA256     string     `json:"sha256"`
}

// Preparing without backend latency keeps setup out of the timed measurement.
// Each part uses distinct data, forcing preflight to inspect many physical packs.
func (f *fixture) prepareSlow(owner credential, path string) {
	f.t.Helper()
	stateFile, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		f.t.Fatal("slow state path must be new")
	}
	defer stateFile.Close()
	s := slowState{Bucket: f.bucket, Credential: owner}
	checkpoint := func() {
		f.t.Helper()
		body, err := json.Marshal(s)
		if err != nil {
			f.t.Fatal("cannot encode slow fixture state")
		}
		if _, err = stateFile.Seek(0, 0); err != nil {
			f.t.Fatal("cannot seek slow state")
		}
		if err = stateFile.Truncate(0); err != nil {
			f.t.Fatal("cannot truncate slow state")
		}
		if _, err = stateFile.Write(body); err != nil {
			f.t.Fatal("cannot persist slow state")
		}
		if err = stateFile.Sync(); err != nil {
			f.t.Fatal("cannot sync slow state")
		}
	}
	checkpoint()
	body, _ := f.s3("slow CreateMultipartUpload", owner, "POST", slowKey, "uploads=", nil, nil, true, 200, "")
	var upload struct {
		ID string `xml:"UploadId"`
	}
	if xml.Unmarshal(body, &upload) != nil || upload.ID == "" {
		f.t.Fatal("missing slow upload identity")
	}
	s.UploadID = upload.ID
	checkpoint()
	hash := sha256.New()
	for part := 1; part <= slowParts; part++ {
		data := bytes.Repeat([]byte{byte(part)}, slowPartBytes)
		hash.Write(data)
		_, headers := f.s3("slow UploadPart", owner, "PUT", slowKey, "uploadId="+url.QueryEscape(s.UploadID)+"&partNumber="+fmt.Sprint(part), data, nil, true, 200, "")
		if headers.Get("ETag") == "" {
			f.t.Fatal("missing slow part ETag")
		}
		s.ETags = append(s.ETags, headers.Get("ETag"))
		checkpoint()
	}
	s.SHA256 = hex.EncodeToString(hash.Sum(nil))
	checkpoint()
}

func (f *fixture) slowOperation(phase string) {
	f.t.Helper()
	var s slowState
	if json.Unmarshal(privateFile(f.t, filepath.Join(f.config.SlowState, f.ep.Name+".json")), &s) != nil ||
		!strings.HasPrefix(s.Bucket, "acceptance-") || s.Credential.ID == "" || s.Credential.Secret == "" || s.UploadID == "" || len(s.ETags) != slowParts || len(s.SHA256) != 64 {
		f.t.Fatal("slow state is incomplete")
	}
	f.bucket = s.Bucket
	if phase == "complete" {
		var body strings.Builder
		body.WriteString("<CompleteMultipartUpload>")
		for i, etag := range s.ETags {
			fmt.Fprintf(&body, "<Part><PartNumber>%d</PartNumber><ETag>", i+1)
			if xml.EscapeText(&body, []byte(etag)) != nil {
				f.t.Fatal("invalid slow part ETag")
			}
			body.WriteString("</ETag></Part>")
		}
		body.WriteString("</CompleteMultipartUpload>")
		response, _ := f.s3("slow CompleteMultipartUpload", s.Credential, "POST", slowKey, "uploadId="+url.QueryEscape(s.UploadID), []byte(body.String()), nil, true, 200, "")
		var complete struct {
			XMLName xml.Name
			ETag    string `xml:"ETag"`
		}
		if xml.Unmarshal(response, &complete) != nil || complete.XMLName.Local != "CompleteMultipartUploadResult" || complete.ETag == "" {
			f.t.Fatal("slow Complete did not return a receipt")
		}
		last := (*f.results)[len(*f.results)-1]
		if last.FirstMS <= 60000 {
			f.t.Fatal("Complete did not exercise a first-response delay exceeding 60 seconds")
		}
		return
	}
	request := f.signed(s.Credential, "GET", slowKey, "", nil, nil, true)
	start := time.Now()
	var first time.Duration
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{GotFirstResponseByte: func() { first = time.Since(start) }}))
	response, err := f.client.Do(request)
	if err != nil {
		f.t.Fatal("slow preflight transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		f.t.Fatalf("slow preflight returned %d", response.StatusCode)
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(response.Body, slowParts*slowPartBytes+1))
	digest := hex.EncodeToString(hash.Sum(nil))
	*f.results = append(*f.results, observation{"slow preflight and full read-back", f.ep.Name, "GET", response.StatusCode, time.Since(start).Milliseconds(), first.Milliseconds(), int(n), digest})
	if err != nil || n != slowParts*slowPartBytes || digest != s.SHA256 {
		f.t.Fatal("slow GET read-back differs")
	}
	if first <= 60*time.Second {
		f.t.Fatal("GET preflight did not exceed 60 seconds before the first response byte")
	}
}
