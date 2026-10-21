package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/kserksi/Pebril/internal/config"
)

const operationTimeout = 2 * time.Minute

type remote struct {
	client        *s3.Client
	configuration config.S3
	namespace     string
}

func newRemote(c config.S3, namespace string, transport aws.HTTPClient) *remote {
	if transport == nil {
		transport = &http.Client{Timeout: 30 * time.Second}
	}
	client := s3.NewFromConfig(aws.Config{Region: c.Region, Credentials: credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, c.SessionToken), HTTPClient: transport, RetryMaxAttempts: 3, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired}, func(o *s3.Options) { o.BaseEndpoint = aws.String(c.Endpoint); o.UsePathStyle = c.Addressing == "path" })
	return &remote{client: client, configuration: c, namespace: namespace}
}
func (r *remote) key(hash string) string { return r.configuration.Prefix + "objects/" + hash }
func (r *remote) marker() string         { return r.configuration.Prefix + "pebril-namespace.json" }
func missing(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && (api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NotFound" || api.ErrorCode() == "404")
}
func precondition(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == "PreconditionFailed"
}
func (r *remote) check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	result, err := r.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.configuration.Bucket), Key: aws.String(r.marker())})
	if err != nil {
		if missing(err) {
			return ErrNamespace
		}
		return ErrUnavailable
	}
	defer result.Body.Close()
	var marker struct {
		Schema    int    `json:"schema"`
		Namespace string `json:"namespace"`
	}
	data, err := io.ReadAll(io.LimitReader(result.Body, 4097))
	if err != nil {
		return ErrUnavailable
	}
	if len(data) > 4096 || json.Unmarshal(data, &marker) != nil || marker.Schema != 1 || marker.Namespace != r.namespace {
		return ErrNamespace
	}
	return nil
}
func (r *remote) claim(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	err := r.check(ctx)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrNamespace) {
		return err
	}
	// A missing marker is the only initialization case; a foreign marker is never overwritten.
	_, err = r.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(r.configuration.Bucket), Key: aws.String(r.marker())})
	if !missing(err) {
		if err == nil {
			return ErrNamespace
		}
		return ErrUnavailable
	}
	listed, err := r.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(r.configuration.Bucket), Prefix: aws.String(r.configuration.Prefix), MaxKeys: aws.Int32(1)})
	if err != nil {
		return ErrUnavailable
	}
	if len(listed.Contents) != 0 || aws.ToBool(listed.IsTruncated) {
		return ErrNamespace
	}
	data, _ := json.Marshal(struct {
		Schema    int    `json:"schema"`
		Namespace string `json:"namespace"`
	}{1, r.namespace})
	_, err = r.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(r.configuration.Bucket), Key: aws.String(r.marker()), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))), IfNoneMatch: aws.String("*"), ContentType: aws.String("application/json")})
	if err != nil && !precondition(err) {
		return ErrUnavailable
	}
	return r.check(ctx)
}
func (r *remote) publish(ctx context.Context, hash string, size int64, md5 string, body io.ReadSeeker) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	_, err := r.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(r.configuration.Bucket), Key: aws.String(r.key(hash)), Body: body, ContentLength: aws.Int64(size), ContentMD5: aws.String(md5), IfNoneMatch: aws.String("*"), Metadata: map[string]string{"pebril-namespace": r.namespace}})
	if err != nil && !precondition(err) {
		return ErrUnavailable
	}
	// Full readback is the generic integrity proof; copied metadata, length and ETag are not hashes.
	result, err := r.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.configuration.Bucket), Key: aws.String(r.key(hash))})
	if err != nil {
		return ErrUnavailable
	}
	defer result.Body.Close()
	if aws.ToInt64(result.ContentLength) != size || result.Metadata["pebril-namespace"] != r.namespace {
		return ErrIntegrity
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(result.Body, size+1))
	if err != nil {
		return ErrUnavailable
	}
	if n != size || hex.EncodeToString(digest.Sum(nil)) != hash {
		return ErrIntegrity
	}
	return nil
}
func (r *remote) open(ctx context.Context, hash string, size int64) (File, error) {
	probe, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	result, err := r.client.HeadObject(probe, &s3.HeadObjectInput{Bucket: aws.String(r.configuration.Bucket), Key: aws.String(r.key(hash))})
	if err != nil {
		if missing(err) {
			return nil, os.ErrNotExist
		}
		return nil, ErrUnavailable
	}
	if aws.ToInt64(result.ContentLength) != size || result.Metadata["pebril-namespace"] != r.namespace {
		return nil, ErrIntegrity
	}
	return &remoteFile{remote: r, ctx: ctx, hash: hash, size: size, etag: result.ETag}, nil
}
func (r *remote) remove(ctx context.Context, hash string) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	head, err := r.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(r.configuration.Bucket), Key: aws.String(r.key(hash))})
	if missing(err) {
		return nil
	}
	if err != nil {
		return ErrUnavailable
	}
	if head.Metadata["pebril-namespace"] != r.namespace {
		return ErrNamespace
	}
	_, err = r.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(r.configuration.Bucket), Key: aws.String(r.key(hash))})
	if err != nil {
		return ErrUnavailable
	}
	return nil
}
func (r *remote) reconcile(ctx context.Context, referenced func(string) bool) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	prefix := r.configuration.Prefix + "objects/"
	var token *string
	for {
		page, err := r.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(r.configuration.Bucket), Prefix: aws.String(prefix), ContinuationToken: token})
		if err != nil {
			return ErrUnavailable
		}
		for _, object := range page.Contents {
			key := aws.ToString(object.Key)
			if !strings.HasPrefix(key, prefix) {
				return ErrNamespace
			}
			hash := strings.TrimPrefix(key, prefix)
			if !ValidHash(hash) || referenced(hash) {
				continue
			}
			if err = r.remove(ctx, hash); err != nil && !errors.Is(err, ErrNamespace) {
				return err
			}
		}
		if !aws.ToBool(page.IsTruncated) {
			return nil
		}
		if page.NextContinuationToken == nil || aws.ToString(page.NextContinuationToken) == aws.ToString(token) {
			return ErrUnavailable
		}
		token = page.NextContinuationToken
	}
}

type remoteFile struct {
	remote       *remote
	ctx          context.Context
	hash         string
	etag         *string
	size, offset int64
	body         io.ReadCloser
	closed       bool
}

func (f *remoteFile) Read(p []byte) (int, error) {
	if f.closed {
		return 0, os.ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	if f.offset >= f.size {
		return 0, io.EOF
	}
	if f.body == nil {
		input := &s3.GetObjectInput{Bucket: aws.String(f.remote.configuration.Bucket), Key: aws.String(f.remote.key(f.hash)), IfMatch: f.etag}
		if f.offset > 0 {
			input.Range = aws.String(fmt.Sprintf("bytes=%d-", f.offset))
		}
		result, err := f.remote.client.GetObject(f.ctx, input)
		if err != nil {
			return 0, ErrUnavailable
		}
		if aws.ToInt64(result.ContentLength) != f.size-f.offset || f.offset > 0 && aws.ToString(result.ContentRange) != fmt.Sprintf("bytes %d-%d/%d", f.offset, f.size-1, f.size) {
			result.Body.Close()
			return 0, ErrIntegrity
		}
		f.body = result.Body
	}
	if int64(len(p)) > f.size-f.offset {
		p = p[:f.size-f.offset]
	}
	n, err := f.body.Read(p)
	f.offset += int64(n)
	if err == io.EOF && f.offset < f.size {
		return n, ErrIntegrity
	}
	return n, err
}
func (f *remoteFile) Seek(offset int64, whence int) (int64, error) {
	if f.closed {
		return 0, os.ErrClosed
	}
	target := offset
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		target += f.offset
	case io.SeekEnd:
		target += f.size
	default:
		return 0, errors.New("Invalid seek origin")
	}
	if target < 0 {
		return 0, errors.New("Invalid seek offset")
	}
	if target != f.offset && f.body != nil {
		f.body.Close()
		f.body = nil
	}
	f.offset = target
	return target, nil
}
func (f *remoteFile) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	if f.body != nil {
		return f.body.Close()
	}
	return nil
}
func (f *remoteFile) Stat() (os.FileInfo, error) {
	if f.closed {
		return nil, os.ErrClosed
	}
	return objectInfo{f.hash, f.size}, nil
}

type objectInfo struct {
	name string
	size int64
}

func (i objectInfo) Name() string       { return i.name }
func (i objectInfo) Size() int64        { return i.size }
func (i objectInfo) Mode() os.FileMode  { return 0400 }
func (i objectInfo) ModTime() time.Time { return time.Time{} }
func (i objectInfo) IsDir() bool        { return false }
func (i objectInfo) Sys() any           { return nil }

func (r *remote) connection(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	_, err := r.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(r.configuration.Bucket), Prefix: aws.String(r.configuration.Prefix), MaxKeys: aws.Int32(1)})
	if err != nil {
		return ErrUnavailable
	}
	return nil
}
