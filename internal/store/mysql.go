package store

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kserksi/Pebril/internal/config"
	"github.com/mattn/go-sqlite3"
)

var ErrDatabaseLost = errors.New("Database connection lost; stop all instances before offline recovery")
var ErrDatabaseOwned = errors.New("Database has an active or unrecovered instance owner; stop all instances before offline recovery")

func mysqlConfiguration(d config.Deployment) (*mysql.Config, error) {
	c, err := mysql.ParseDSN(d.MySQLDSN)
	if err != nil {
		return nil, errors.New("Invalid PEBRIL_MYSQL_DSN")
	}
	if c.DBName == "" || c.User == "" || c.MultiStatements || c.AllowFallbackToPlaintext || c.AllowAllFiles || c.AllowCleartextPasswords || c.AllowOldPasswords || c.RejectReadOnly || c.InterpolateParams || c.Params["allowNativePasswords"] == "false" {
		return nil, errors.New("Unsafe or incomplete MySQL connection configuration")
	}
	for key := range c.Params {
		if key != "charset" {
			return nil, errors.New("Unsupported MySQL session parameter")
		}
	}
	if c.TLSConfig != "" && c.TLSConfig != "true" {
		return nil, errors.New("MySQL TLS must verify certificates without downgrade")
	}
	if c.Net != "tcp" && c.Net != "unix" {
		return nil, errors.New("MySQL supports TCP or a local Unix socket")
	}
	if c.Net == "tcp" {
		c.TLSConfig = "true"
		c.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if d.MySQLCAFile != "" {
		data, e := os.ReadFile(d.MySQLCAFile)
		if e != nil {
			return nil, errors.New("Cannot read MySQL CA file")
		}
		roots, e := x509.SystemCertPool()
		if e != nil {
			return nil, errors.New("Cannot load certificate trust roots")
		}
		if !roots.AppendCertsFromPEM(data) {
			return nil, errors.New("MySQL CA file contains no usable certificate")
		}
		c.TLSConfig = "true"
		c.TLS = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	}
	c.Timeout = 15 * time.Second
	c.ReadTimeout = 15 * time.Second
	c.WriteTimeout = 15 * time.Second
	c.Logger = quietMySQLLog{}
	c.ClientFoundRows = true
	c.Collation = "utf8mb4_0900_as_cs"
	c.Params = map[string]string{"sql_mode": "'STRICT_ALL_TABLES,NO_ZERO_DATE,NO_ENGINE_SUBSTITUTION'", "transaction_isolation": "'SERIALIZABLE'"}
	return c, nil
}

// A lost physical session must never be replaced with a connection lacking the instance lock.
type instanceConnector struct {
	driver.Connector
	mu     sync.Mutex
	used   bool
	onLost func()
}

func (c *instanceConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used {
		c.onLost()
		return nil, ErrDatabaseLost
	}
	c.used = true
	return c.Connector.Connect(ctx)
}
func openMySQL(d config.Deployment, recovery, existingOnly bool) (*Store, error) {
	c, err := mysqlConfiguration(d)
	if err != nil {
		return nil, err
	}
	lock, err := localLock(d.DBDir)
	if err != nil {
		return nil, err
	}
	s := &Store{lock: lock, backend: "mysql", Started: time.Now(), lost: make(chan struct{})}
	fail := func(e error) (*Store, error) { s.Close(); return nil, e }
	base, err := mysql.NewConnector(c)
	if err != nil {
		return fail(errors.New("Invalid MySQL connection configuration"))
	}
	connector := &instanceConnector{Connector: base, onLost: s.markLost}
	s.DB = sql.OpenDB(connector)
	s.DB.SetMaxOpenConns(1)
	s.DB.SetMaxIdleConns(1)
	if err = s.DB.Ping(); err != nil {
		return fail(errors.New("Cannot connect to selected MySQL database"))
	}
	var version string
	if err = s.DB.QueryRow("SELECT VERSION()").Scan(&version); err != nil || !strings.HasPrefix(version, "8.4.") {
		return fail(errors.New("Selected database must be MySQL 8.4 LTS"))
	}
	sum := sha256.Sum256([]byte(c.DBName))
	name := "pebril:" + hex.EncodeToString(sum[:28])
	var held sql.NullInt64
	if err = s.DB.QueryRow("SELECT GET_LOCK(?,0)", name).Scan(&held); err != nil || !held.Valid || held.Int64 != 1 {
		return fail(errors.New("Selected MySQL database is already in use or cannot acquire its instance lock"))
	}
	if !recovery {
		var previous string
		e := s.DB.QueryRow("SELECT owner FROM deployment WHERE id=1").Scan(&previous)
		if e == nil && previous != "" {
			return fail(ErrDatabaseOwned)
		}
	}
	if err = s.mysqlSchema(existingOnly, recovery); err != nil {
		return fail(err)
	}
	var owner string
	if err = s.DB.QueryRow("SELECT namespace,owner FROM deployment WHERE id=1").Scan(&s.namespace, &owner); err != nil {
		return fail(errors.New("Cannot read database instance identity"))
	}
	if recovery {
		return s, nil
	}
	if owner != "" {
		return fail(ErrDatabaseOwned)
	}
	owner, err = identity()
	if err != nil {
		return fail(err)
	}
	result, err := s.DB.Exec("UPDATE deployment SET owner=? WHERE id=1 AND owner=''", owner)
	if err != nil {
		return fail(errors.New("Cannot claim database instance ownership"))
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return fail(ErrDatabaseOwned)
	}
	s.owner = owner
	return s, nil
}

func (s *Store) mysqlSchema(existingOnly, recovery bool) error {
	var tables int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tables); err != nil {
		return errors.New("Cannot inspect selected database schema")
	}
	if tables != 0 {
		var version int
		if err := s.DB.QueryRow("SELECT COALESCE(MAX(version),0) FROM schema_migrations").Scan(&version); err != nil || version < 4 || version > 5 {
			return errors.New("Unsupported, unrelated or incomplete MySQL schema; restore a matching backup or recreate the owned empty target")
		}
		if version == 5 && tables != 8 || version == 4 && tables != 7 {
			return errors.New("MySQL schema table inventory does not match the supported version")
		}
		for _, query := range []string{
			"SELECT id,username,password,admin,status,quota,used,administrator FROM users LIMIT 0",
			"SELECT hash,user_id,csrf,expires FROM sessions LIMIT 0",
			"SELECT user_id,hash FROM upload_tokens LIMIT 0",
			"SELECT id,user_id,state,policy,created_at,format,bytes,width,height,frames,error,hash,generation,attempts,next_run,fallback,delivered FROM images LIMIT 0",
			"SELECT image_id,role,hash,bytes,format,width,height,frames FROM image_files LIMIT 0",
			"SELECT id,namespace,owner FROM deployment LIMIT 0",
		} {
			rows, err := s.DB.Query(query)
			if err != nil {
				return errors.New("MySQL schema fields do not match the supported version")
			}
			rows.Close()
		}
		if version == 4 && !recovery {
			if _, err := s.DB.Exec(mysqlObjectKeysDDL); err != nil {
				return errors.New("Object-registry schema upgrade failed; retain the matching backup before retrying")
			}
			if _, err := s.DB.Exec("ALTER TABLE deployment ADD COLUMN storage_backend VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT ''"); err != nil {
				return errors.New("Storage-binding schema upgrade failed; restore the matching backup before retrying")
			}
			if _, err := s.DB.Exec("INSERT INTO schema_migrations VALUES(5,?)", time.Now().UTC().UnixMilli()); err != nil {
				return errors.New("Object-registry schema version update failed; retry only after checking the stopped database")
			}
		}
		if version == 5 || version == 4 && !recovery {
			rows, err := s.DB.Query("SELECT o.hash,o.generation,o.active_generation,d.storage_backend FROM object_keys o CROSS JOIN deployment d LIMIT 0")
			if err != nil {
				return errors.New("Invalid object-registry schema")
			}
			rows.Close()
		}
		return nil
	}
	if existingOnly {
		return errors.New("Recovery requires an existing initialized MySQL database")
	}
	statements := []string{
		`CREATE TABLE schema_migrations(version BIGINT PRIMARY KEY,applied_at BIGINT NOT NULL) ENGINE=InnoDB`,
		`CREATE TABLE users(id BIGINT PRIMARY KEY AUTO_INCREMENT,username VARBINARY(64) NOT NULL UNIQUE,password BLOB NOT NULL,admin TINYINT NOT NULL DEFAULT 0 CHECK(admin IN(0,1)),status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'active',quota BIGINT NOT NULL,used BIGINT NOT NULL DEFAULT 0,administrator TINYINT GENERATED ALWAYS AS (CASE WHEN admin=1 THEN 1 ELSE NULL END) STORED UNIQUE) ENGINE=InnoDB`,
		`CREATE TABLE sessions(hash VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,user_id BIGINT NOT NULL,csrf VARCHAR(43) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,expires BIGINT NOT NULL,FOREIGN KEY(user_id) REFERENCES users(id)) ENGINE=InnoDB`,
		`CREATE TABLE upload_tokens(user_id BIGINT PRIMARY KEY,hash VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE,FOREIGN KEY(user_id) REFERENCES users(id)) ENGINE=InnoDB`,
		`CREATE TABLE images(id VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,user_id BIGINT NOT NULL,state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,policy LONGBLOB NOT NULL,created_at BIGINT NOT NULL,format VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',bytes BIGINT NOT NULL DEFAULT 0,width BIGINT NOT NULL DEFAULT 0,height BIGINT NOT NULL DEFAULT 0,frames BIGINT NOT NULL DEFAULT 0,error TEXT NOT NULL,hash VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',generation BIGINT NOT NULL DEFAULT 0,attempts BIGINT NOT NULL DEFAULT 0,next_run BIGINT NOT NULL DEFAULT 0,fallback TINYINT NOT NULL DEFAULT 0,delivered BIGINT NOT NULL DEFAULT 0,FOREIGN KEY(user_id) REFERENCES users(id),INDEX image_tasks(state,user_id),INDEX conversion_schedule(state,next_run,user_id,created_at)) ENGINE=InnoDB`,
		`CREATE TABLE image_files(image_id VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,role VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,hash VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,bytes BIGINT NOT NULL,format VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,width BIGINT NOT NULL,height BIGINT NOT NULL,frames BIGINT NOT NULL,PRIMARY KEY(image_id,role),FOREIGN KEY(image_id) REFERENCES images(id) ON DELETE CASCADE,INDEX image_file_hash(hash)) ENGINE=InnoDB`,
		`CREATE TABLE deployment(id BIGINT PRIMARY KEY CHECK(id=1),namespace VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,owner VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',storage_backend VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '') ENGINE=InnoDB`,
		mysqlObjectKeysDDL,
	}
	for _, statement := range statements {
		if _, err := s.DB.Exec(statement); err != nil {
			return errors.New("MySQL schema initialization interrupted; restore or recreate the owned target before retrying")
		}
	}
	namespace, err := identity()
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO deployment(id,namespace) VALUES(1,?)", namespace); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO schema_migrations VALUES(5,?)", time.Now().UTC().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

func RecoverDatabase(d config.Deployment, confirm func() error) error {
	if d.DatabaseBackend != "mysql" {
		return errors.New("Offline database-owner recovery is only needed for MySQL")
	}
	s, err := openMySQL(d, true, true)
	if err != nil {
		return err
	}
	defer s.Close()
	if err = confirm(); err != nil {
		return err
	}
	_, err = s.DB.Exec("UPDATE deployment SET owner='' WHERE id=1")
	return err
}
func CheckDeployment(d config.Deployment) error {
	if d.DatabaseBackend != "mysql" {
		return nil
	}
	c, err := mysqlConfiguration(d)
	if err != nil {
		return err
	}
	connector, err := mysql.NewConnector(c)
	if err != nil {
		return errors.New("Invalid MySQL connection configuration")
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	var version string
	if err = db.QueryRow("SELECT VERSION()").Scan(&version); err != nil || !strings.HasPrefix(version, "8.4.") {
		return errors.New("Selected MySQL 8.4 database is unavailable or incompatible")
	}
	return nil
}
func (s *Store) subtractUsageSQL() string {
	if s.Backend() == "mysql" {
		return "UPDATE users SET used=GREATEST(0,used-?) WHERE id=?"
	}
	return "UPDATE users SET used=MAX(0,used-?) WHERE id=?"
}
func IsUnique(err error) bool {
	var sqlite sqlite3.Error
	var server *mysql.MySQLError
	return errors.As(err, &sqlite) && sqlite.ExtendedCode == sqlite3.ErrConstraintUnique || errors.As(err, &server) && server.Number == 1062
}

func OpenExistingDeployment(d config.Deployment) (*Store, error) {
	if d.DatabaseBackend == "mysql" {
		return openMySQL(d, false, true)
	}
	return Open(d.DBDir)
}

type quietMySQLLog struct{}

func (quietMySQLLog) Print(...any) {}

const mysqlObjectKeysDDL = "CREATE TABLE IF NOT EXISTS object_keys(hash VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,generation BIGINT NOT NULL CHECK(generation>=0),active_generation BIGINT NULL,CHECK(active_generation IS NULL OR (active_generation>=0 AND active_generation<=generation))) ENGINE=InnoDB"
