// Package testutil supplies controlled test doubles; it is not linked into application binaries.
package testutil

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type S3Object struct {
	Data  []byte
	Owner string
}
type S3Fixture struct {
	Mu        sync.Mutex
	Objects   map[string]S3Object
	Ranges    []string
	Lists     int
	Corrupt   string
	Denied    bool
	Uncertain bool
}

func (f *S3Fixture) serve(w http.ResponseWriter, r *http.Request) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=test-access/") {
		http.Error(w, "signature missing", 403)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/images/")
	if r.URL.Query().Get("list-type") == "2" {
		f.Lists++
		prefix := r.URL.Query().Get("prefix")
		var keys []string
		for key := range f.Objects {
			if strings.HasPrefix(key, prefix) {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		after := r.URL.Query().Get("continuation-token")
		start := sort.SearchStrings(keys, after)
		if start < len(keys) && keys[start] == after {
			start++
		}
		maximum := 2
		if r.URL.Query().Get("max-keys") == "1" {
			maximum = 1
		}
		end := min(start+maximum, len(keys))
		next := ""
		if end < len(keys) {
			next = keys[end-1]
		}
		result := struct {
			XMLName  xml.Name `xml:"ListBucketResult"`
			Contents []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
			Truncated bool   `xml:"IsTruncated"`
			Next      string `xml:"NextContinuationToken,omitempty"`
		}{Truncated: next != "", Next: next}
		for _, key := range keys[start:end] {
			result.Contents = append(result.Contents, struct {
				Key string `xml:"Key"`
			}{key})
		}
		w.Header().Set("Content-Type", "application/xml")
		xml.NewEncoder(w).Encode(result)
		return
	}
	object, exists := f.Objects[key]
	fail := func(status int, code string) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		if r.Method != "HEAD" {
			fmt.Fprintf(w, "<Error><Code>%s</Code></Error>", code)
		}
	}
	if f.Denied {
		fail(403, "AccessDenied")
		return
	}
	switch r.Method {
	case "PUT":
		if exists && r.Header.Get("If-None-Match") == "*" {
			fail(412, "PreconditionFailed")
			return
		}
		Data, err := io.ReadAll(r.Body)
		if err != nil {
			fail(400, "IncompleteBody")
			return
		}
		f.Objects[key] = S3Object{Data: Data, Owner: r.Header.Get("X-Amz-Meta-Pebril-Namespace")}
		if f.Uncertain && strings.Contains(key, "/objects/") {
			f.Uncertain = false
			fail(500, "InternalError")
			return
		}
		digest := md5.Sum(Data)
		w.Header().Set("ETag", `"`+hex.EncodeToString(digest[:])+`"`)
		w.WriteHeader(200)
	case "HEAD", "GET":
		if !exists {
			fail(404, "NoSuchKey")
			return
		}
		digest := md5.Sum(object.Data)
		etag := `"` + hex.EncodeToString(digest[:]) + `"`
		if value := r.Header.Get("If-Match"); value != "" && value != etag {
			fail(412, "PreconditionFailed")
			return
		}
		Data := object.Data
		if key == f.Corrupt {
			Data = append([]byte(nil), Data...)
			Data[0] ^= 1
		}
		status := 200
		if value := r.Header.Get("Range"); value != "" {
			f.Ranges = append(f.Ranges, value)
			var start int
			if _, err := fmt.Sscanf(value, "bytes=%d-", &start); err != nil || start >= len(Data) {
				fail(416, "InvalidRange")
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(Data)-1, len(Data)))
			Data = Data[start:]
			status = 206
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(Data)))
		w.Header().Set("ETag", etag)
		if object.Owner != "" {
			w.Header().Set("X-Amz-Meta-Pebril-Namespace", object.Owner)
		}
		w.WriteHeader(status)
		if r.Method == "GET" {
			w.Write(Data)
		}
	case "DELETE":
		delete(f.Objects, key)
		w.WriteHeader(204)
	default:
		fail(405, "MethodNotAllowed")
	}
}

func NewS3(t *testing.T) (*S3Fixture, *httptest.Server) {
	t.Helper()
	fixture := &S3Fixture{Objects: map[string]S3Object{}}
	server := httptest.NewTLSServer(http.HandlerFunc(fixture.serve))
	t.Cleanup(server.Close)
	return fixture, server
}
