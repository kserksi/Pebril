package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageIsolationAndAtomicReload(t *testing.T) {
	d := testDeployment(t)
	path := filepath.Join(d.Root, "config.json")
	storage := Storage{Backend: "s3", S3: &S3{Endpoint: "https://objects.example.test", Bucket: "images", Region: "test-region", AccessKeyID: "private-access", SecretAccessKey: "private-secret", SessionToken: "private-session", Addressing: "path", Prefix: "application/"}}
	write := func(s Storage, quality int) {
		t.Helper()
		data, err := json.Marshal(diskConfiguration{Runtime: func() Runtime { r := Defaults(); r.StaticQuality = quality; return r }(), Storage: s})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(storage, 80)
	manager, err := New(d, false)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	data, _ := json.Marshal(manager.Snapshot())
	for _, secret := range []string{"private-access", "private-secret", "private-session", "objects.example", "storage"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("Processing snapshot contains storage configuration")
		}
	}
	clone := manager.Storage()
	clone.S3.SecretAccessKey = "changed"
	if manager.Storage().S3.SecretAccessKey != "private-secret" {
		t.Fatal("Caller mutated active storage")
	}
	write(storage, 75)
	if err = manager.Reload(); err != nil {
		t.Fatal(err)
	}
	revision := manager.Snapshot().Revision
	storage.S3.Endpoint = "https://other.example.test"
	write(storage, 50)
	if err = manager.Reload(); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatal("Connection changed without restart", err)
	}
	if manager.Snapshot().Revision != revision || manager.Snapshot().Policy.StaticQuality != 75 || manager.Storage().S3.Endpoint != "https://objects.example.test" {
		t.Fatal("Mixed reload was partially applied")
	}
	write(Storage{Backend: "local"}, 30)
	if err = manager.Reload(); err == nil {
		t.Fatal("Backend switched without restart")
	}
}

func TestStorageValidationDoesNotEchoSecrets(t *testing.T) {
	valid := S3{Endpoint: "https://objects.example.test", Bucket: "images", Region: "region", AccessKeyID: "access", SecretAccessKey: "private-secret", Addressing: "virtual", Prefix: "owned/"}
	for _, edit := range []func(*S3){func(s *S3) { s.Endpoint = "https://user:private-secret@host/" }, func(s *S3) { s.Endpoint = "http://host/" }, func(s *S3) { s.Endpoint = "https://host/?token=private-secret" }, func(s *S3) { s.Addressing = "auto" }, func(s *S3) { s.Prefix = "" }, func(s *S3) { s.Prefix = "../owned/" }, func(s *S3) { s.Prefix = "a//b/" }, func(s *S3) { s.Region = "" }, func(s *S3) { s.SecretAccessKey = "" }} {
		c := valid
		edit(&c)
		err := (Storage{Backend: "s3", S3: &c}).Validate()
		if err == nil {
			t.Fatal("Invalid storage accepted")
		}
		if strings.Contains(err.Error(), "private-secret") {
			t.Fatal("Credential disclosed")
		}
	}
	if err := (Storage{Backend: "s3", S3: &valid}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateConfigurationAndIndependentMigrationRoots(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, ".env"), []byte("PEBRIL_DATABASE_BACKEND=sqlite\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, ".env"), []byte("PEBRIL_DATABASE_BACKEND=mysql\nPEBRIL_MYSQL_DSN=private-dsn\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PEBRIL_DATABASE_BACKEND", "mysql")
	t.Setenv("PEBRIL_MYSQL_DSN", "inherited-secret")
	a, err := LoadDeploymentFile(source)
	if err != nil || a.DatabaseBackend != "sqlite" || a.MySQLDSN != "" {
		t.Fatal("Inherited deployment variables mixed source/target roots", err)
	}
	b, err := LoadDeploymentFile(target)
	if err != nil || b.MySQLDSN != "private-dsn" {
		t.Fatal("Target file configuration was overridden", err)
	}
	raw, _ := json.Marshal(b)
	if strings.Contains(string(raw), "private-dsn") {
		t.Fatal("Deployment serialization exposed database credentials")
	}
	if err = os.Chmod(filepath.Join(target, ".env"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadDeploymentFile(target); err == nil || strings.Contains(err.Error(), "private-dsn") {
		t.Fatal("Readable database credential file was accepted or disclosed", err)
	}
}
