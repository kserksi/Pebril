package store

import (
	"github.com/kserksi/Pebril/internal/config"
	"strings"
	"testing"
)

func TestMySQLConnectionValidationAndSecretRedaction(t *testing.T) {
	for _, query := range []string{"tls=skip-verify", "tls=preferred", "tls=false", "allowFallbackToPlaintext=true", "allowCleartextPasswords=true", "allowOldPasswords=true", "multiStatements=true", "allowAllFiles=true", "interpolateParams=true", "sql_mode=ANSI"} {
		_, err := mysqlConfiguration(config.Deployment{MySQLDSN: "user:private-secret@tcp(database.example.invalid:3306)/images?" + query})
		if err == nil {
			t.Fatal("Unsafe MySQL option accepted", query)
		}
		if strings.Contains(err.Error(), "private-secret") || strings.Contains(err.Error(), "database.example") {
			t.Fatal("MySQL configuration error disclosed connection details")
		}
	}
	c, err := mysqlConfiguration(config.Deployment{MySQLDSN: "user:private-secret@tcp(database.example.invalid:3306)/images?tls=true"})
	if err != nil || c.TLS == nil || c.TLS.InsecureSkipVerify || c.AllowFallbackToPlaintext || !c.ClientFoundRows {
		t.Fatal("Verified TCP TLS or affected-row contract changed", err)
	}
	if _, err = mysqlConfiguration(config.Deployment{MySQLDSN: "user:private-secret@unix(/private/mysql.sock)/images"}); err != nil {
		t.Fatal("Private local Unix socket refused", err)
	}
}
