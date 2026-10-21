package testutil

import (
	"database/sql"
	"github.com/go-sql-driver/mysql"
	"github.com/joho/godotenv"
	"github.com/kserksi/Pebril/internal/config"
	"os"
	"strings"
	"testing"
)

// MySQLDeployment uses a separate, explicitly provisioned disposable database per test lane.
func MySQLDeployment(t *testing.T, d config.Deployment, lane string) config.Deployment {
	t.Helper()
	file := os.Getenv("PEBRIL_TEST_MYSQL_ENV")
	if file == "" {
		t.Skip("Real MySQL lane not configured; this is not interoperability acceptance")
	}
	values, err := godotenv.Read(file)
	if err != nil {
		t.Fatal("Cannot read private MySQL test configuration")
	}
	c, err := mysql.ParseDSN(values["PEBRIL_MYSQL_DSN"])
	if err != nil || !strings.HasPrefix(c.DBName, "pebril_test_") {
		t.Fatal("Disposable MySQL database required")
	}
	for _, ch := range lane {
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') {
			t.Fatal("Invalid test lane")
		}
	}
	if lane == "" {
		t.Fatal("Test lane required")
	}
	c.DBName += "_" + lane
	control, err := sql.Open("mysql", c.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	var count int
	if err = control.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&count); err != nil || count != 0 {
		t.Fatal("MySQL fixture must be an empty owned database", err)
	}
	t.Cleanup(func() {
		rows, e := control.Query("SELECT table_name FROM information_schema.tables WHERE table_schema=DATABASE()")
		if e != nil {
			t.Error(e)
			return
		}
		var tables []string
		for rows.Next() {
			var name string
			if e = rows.Scan(&name); e != nil {
				t.Error(e)
				break
			}
			tables = append(tables, name)
		}
		if e = rows.Err(); e != nil {
			t.Error(e)
		}
		rows.Close()
		owned := map[string]bool{"image_files": true, "images": true, "sessions": true, "upload_tokens": true, "users": true, "object_keys": true, "deployment": true, "schema_migrations": true}
		for _, name := range tables {
			if !owned[name] {
				t.Error("Unknown table; refusing fixture cleanup")
				return
			}
		}
		for _, name := range []string{"image_files", "images", "sessions", "upload_tokens", "users", "object_keys", "deployment", "schema_migrations"} {
			if _, e = control.Exec("DROP TABLE IF EXISTS " + name); e != nil {
				t.Error(e)
			}
		}
	})
	d.DatabaseBackend = "mysql"
	d.MySQLDSN = c.FormatDSN()
	return d
}
