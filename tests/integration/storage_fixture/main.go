// Command storage_fixture inspects and clears one isolated S3 namespace prefix
// for the upload integration test. It is test-only tooling: it is never built
// into the product, it refuses to act on an empty prefix, and it only reads a
// private connection file whose values are never printed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type connection struct {
	Endpoint        string `json:"endpoint"`
	Bucket          string `json:"bucket"`
	Region          string `json:"region"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Addressing      string `json:"addressing"`
}

type fixture struct {
	client *s3.Client
	bucket string
	prefix string
}

func main() {
	configPath := flag.String("config", "", "private JSON connection file")
	prefix := flag.String("prefix", "", "isolated object prefix ending in /")
	flag.Parse()
	command := flag.Args()
	if len(command) == 0 {
		fail("expected one of count, cleanup, exists <sha256>, or marker")
	}
	if *configPath == "" || !validPrefix(*prefix) {
		fail("a private connection file and a normalized prefix ending in / are required")
	}
	raw, err := os.ReadFile(*configPath)
	if err != nil {
		fail("read connection file: %v", err)
	}
	var c connection
	if err := json.Unmarshal(raw, &c); err != nil {
		fail("parse connection file: %v", err)
	}
	if c.Endpoint == "" || c.Bucket == "" || c.Region == "" || c.AccessKeyID == "" || c.SecretAccessKey == "" {
		fail("connection file is incomplete")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f := &fixture{client: newClient(ctx, c), bucket: c.Bucket, prefix: *prefix}
	switch command[0] {
	case "count":
		f.count(ctx)
	case "cleanup":
		f.cleanup(ctx)
	case "exists":
		if len(command) != 2 || len(command[1]) != 64 || strings.ToLower(command[1]) != command[1] {
			fail("exists needs one lowercase sha256 digest")
		}
		f.exists(ctx, command[1])
	case "marker":
		f.marker(ctx)
	default:
		fail("unknown command %q", command[0])
	}
}

func validPrefix(prefix string) bool {
	if prefix == "" || !strings.HasSuffix(prefix, "/") || strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "//") || strings.Contains(prefix, "..") || strings.ContainsAny(prefix, "\\\x00\r\n") {
		return false
	}
	return true
}

func newClient(_ context.Context, c connection) *s3.Client {
	configuration := aws.Config{Region: c.Region, Credentials: credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, ""), RetryMaxAttempts: 3}
	return s3.NewFromConfig(configuration, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(c.Endpoint)
		o.UsePathStyle = c.Addressing != "virtual"
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
}

func (f *fixture) keys(ctx context.Context, prefix string) []types.Object {
	var objects []types.Object
	var token *string
	for {
		page, err := f.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(f.bucket), Prefix: aws.String(prefix), ContinuationToken: token, MaxKeys: aws.Int32(1000)})
		if err != nil {
			fail("list %q: %v", prefix, err)
		}
		objects = append(objects, page.Contents...)
		if page.IsTruncated == nil || !*page.IsTruncated {
			return objects
		}
		token = page.NextContinuationToken
	}
}

func (f *fixture) count(ctx context.Context) {
	objects := f.keys(ctx, f.prefix)
	var bytes int64
	for _, object := range objects {
		bytes += aws.ToInt64(object.Size)
	}
	emit(map[string]any{"objects": len(objects), "bytes": bytes})
}

func (f *fixture) cleanup(ctx context.Context) {
	deleted := 0
	for {
		objects := f.keys(ctx, f.prefix)
		if len(objects) == 0 {
			break
		}
		if len(objects) > 1000 {
			objects = objects[:1000]
		}
		identifiers := make([]types.ObjectIdentifier, 0, len(objects))
		for _, object := range objects {
			identifiers = append(identifiers, types.ObjectIdentifier{Key: object.Key})
		}
		if _, err := f.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(f.bucket), Delete: &types.Delete{Objects: identifiers, Quiet: aws.Bool(true)}}); err != nil {
			fail("delete: %v", err)
		}
		deleted += len(identifiers)
	}
	remaining := len(f.keys(ctx, f.prefix))
	emit(map[string]any{"deleted": deleted, "remaining": remaining})
	if remaining != 0 {
		os.Exit(3)
	}
}

func (f *fixture) exists(ctx context.Context, digest string) {
	objects := f.keys(ctx, f.prefix+"objects/"+digest)
	emit(map[string]any{"exists": len(objects) > 0, "objects": len(objects)})
	if len(objects) == 0 {
		os.Exit(3)
	}
}

func (f *fixture) marker(ctx context.Context) {
	_, err := f.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(f.bucket), Key: aws.String(f.prefix + "pebril-namespace.json")})
	if err != nil {
		var missing *types.NotFound
		if errors.As(err, &missing) {
			emit(map[string]any{"marker": false})
			os.Exit(3)
		}
		fail("head marker: %v", err)
	}
	emit(map[string]any{"marker": true})
}

func emit(value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		fail("encode result: %v", err)
	}
	fmt.Println(string(encoded))
}

func fail(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, "storage_fixture: "+format+"\n", arguments...)
	os.Exit(2)
}
