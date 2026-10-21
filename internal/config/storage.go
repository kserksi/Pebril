package config

import (
	"errors"
	"net/url"
	"path"
	"strings"
)

// Storage is deployment configuration and must never enter a persisted processing snapshot.
type Storage struct {
	Backend string `json:"backend"`
	S3      *S3    `json:"s3,omitempty"`
}
type S3 struct {
	Endpoint        string `json:"endpoint"`
	Bucket          string `json:"bucket"`
	Region          string `json:"region"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token,omitempty"`
	Addressing      string `json:"addressing"`
	Prefix          string `json:"prefix"`
}
type diskConfiguration struct {
	Runtime
	Storage Storage `json:"storage"`
}

func (s S3) String() string { return "S3 connection (credentials redacted)" }
func (s Storage) Validate() error {
	if s.Backend != "local" && s.Backend != "s3" {
		return errors.New("storage.backend must be local or s3")
	}
	if s.S3 == nil {
		if s.Backend == "s3" {
			return errors.New("storage.s3 is required for S3")
		}
		return nil
	}
	c := s.S3
	endpoint, err := url.Parse(c.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Opaque != "" {
		return errors.New("storage.s3.endpoint must be an HTTPS URL without credentials, query or fragment")
	}
	if c.Bucket == "" || strings.ContainsAny(c.Bucket, "/\\\x00\r\n") || c.Region == "" || strings.TrimSpace(c.Region) != c.Region || c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return errors.New("storage.s3 requires bucket, region and explicit paired credentials")
	}
	if c.Addressing != "path" && c.Addressing != "virtual" {
		return errors.New("storage.s3.addressing must be path or virtual")
	}
	prefix := strings.TrimSuffix(c.Prefix, "/")
	if prefix == "" || !strings.HasSuffix(c.Prefix, "/") || strings.HasPrefix(prefix, "/") || path.Clean(prefix) != prefix || strings.ContainsAny(prefix, "\\\x00\r\n") {
		return errors.New("storage.s3.prefix must be a nonempty normalized relative namespace ending in /")
	}
	for _, segment := range strings.Split(prefix, "/") {
		if segment == "." || segment == ".." {
			return errors.New("storage.s3.prefix has an invalid segment")
		}
	}
	return nil
}

func (m *Manager) Storage() Storage {
	s := m.storage
	if s.S3 != nil {
		copy := *s.S3
		s.S3 = &copy
	}
	return s
}
