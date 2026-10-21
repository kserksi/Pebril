package migration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/objectstore"
	"github.com/kserksi/Pebril/internal/store"
)

// This is deliberately limited to the three supported stopped SQLite/local forward paths.
type table struct {
	name, columns, order string
	binary               map[string]bool
	numbers              map[string]bool
}

var tables = []table{
	{name: "users", columns: "id,username,password,admin,status,quota,used", order: "id", binary: map[string]bool{"password": true}, numbers: map[string]bool{"id": true, "admin": true, "quota": true, "used": true}},
	{name: "sessions", columns: "hash,user_id,csrf,expires", order: "hash", numbers: map[string]bool{"user_id": true, "expires": true}},
	{name: "upload_tokens", columns: "user_id,hash", order: "user_id", numbers: map[string]bool{"user_id": true}},
	{name: "images", columns: "id,user_id,state,policy,created_at,format,bytes,width,height,frames,error,hash,generation,attempts,next_run,fallback,delivered", order: "id", binary: map[string]bool{"policy": true}, numbers: map[string]bool{"user_id": true, "created_at": true, "bytes": true, "width": true, "height": true, "frames": true, "generation": true, "attempts": true, "next_run": true, "fallback": true, "delivered": true}},
	{name: "image_files", columns: "image_id,role,hash,bytes,format,width,height,frames", order: "image_id,role", numbers: map[string]bool{"bytes": true, "width": true, "height": true, "frames": true}},
}

type content struct {
	hash string
	size int64
	path string
}
type receipt struct {
	Version   int    `json:"version"`
	Source    string `json:"source"`
	Target    string `json:"target"`
	Namespace string `json:"namespace"`
	Phase     string `json:"phase"`
	Records   int    `json:"records"`
	Objects   int    `json:"objects"`
}
type Options struct {
	Source, Target               config.Deployment
	SourceStorage, TargetStorage config.Storage
	Progress                     io.Writer
}

func readTables(db *sql.DB, version int) ([][][]any, error) {
	result := make([][][]any, len(tables))
	for n, t := range tables {
		columns := t.columns
		if t.name == "images" && version == 3 {
			columns = strings.ReplaceAll(columns, ",delivered", ",0 AS delivered")
		}
		rows, err := db.Query("SELECT " + columns + " FROM " + t.name + " ORDER BY " + t.order)
		if err != nil {
			return nil, errors.New("Cannot read migration record inventory")
		}
		names := strings.Split(t.columns, ",")
		for rows.Next() {
			values := make([]any, len(names))
			dest := make([]any, len(names))
			for i := range values {
				dest[i] = &values[i]
			}
			if err = rows.Scan(dest...); err != nil {
				rows.Close()
				return nil, err
			}
			for i, name := range names {
				if values[i] == nil {
					rows.Close()
					return nil, errors.New("Migration encountered an unexpected null record value")
				}
				if t.binary[name] {
					switch value := values[i].(type) {
					case []byte:
						values[i] = append([]byte(nil), value...)
					case string:
						values[i] = []byte(value)
					default:
						rows.Close()
						return nil, errors.New("Invalid binary source field")
					}
				}
				if t.numbers[name] {
					switch value := values[i].(type) {
					case int64:
					case []byte:
						values[i], err = strconv.ParseInt(string(value), 10, 64)
					default:
						err = errors.New("Invalid integer source field")
					}
					if err != nil {
						rows.Close()
						return nil, errors.New("Invalid exact integer source field")
					}
				}
				if !t.binary[name] && !t.numbers[name] {
					switch value := values[i].(type) {
					case string:
					case []byte:
						values[i] = string(value)
					default:
						rows.Close()
						return nil, errors.New("Invalid text source field")
					}
				}
			}
			result[n] = append(result[n], values)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}
func digest(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func inventory(records [][][]any, source config.Deployment) ([]content, []content, error) {
	objects := map[string]int64{}
	charges := map[string]map[string]int64{}
	owners := map[string]int64{}
	used := map[int64]int64{}
	for _, user := range records[0] {
		used[user[0].(int64)] = user[6].(int64)
	}
	var received []content
	for _, image := range records[3] {
		id := image[0].(string)
		raw, err := hex.DecodeString(id)
		if err != nil || len(raw) != 16 || hex.EncodeToString(raw) != id {
			return nil, nil, errors.New("Invalid source image identifier")
		}
		state := image[2].(string)
		if state == "receiving" {
			return nil, nil, errors.New("Source contains incomplete reception; restart the source to reconcile it, then stop and back up again")
		}
		owners[id] = image[1].(int64)
		if state == "received" {
			path := filepath.Join(source.DataDir, ".incoming", id, "input")
			file, err := regular(path)
			if err != nil {
				return nil, nil, errors.New("Fully received source input is missing or unsafe")
			}
			hash := sha256.New()
			count, err := io.Copy(hash, file)
			file.Close()
			if err != nil {
				return nil, nil, err
			}
			received = append(received, content{hex.EncodeToString(hash.Sum(nil)), count, path})
		}
	}
	for _, file := range records[4] {
		id, hash, size := file[0].(string), file[2].(string), file[3].(int64)
		if !objectstore.ValidHash(hash) || size <= 0 {
			return nil, nil, errors.New("Invalid source object reference")
		}
		if previous, exists := objects[hash]; exists && previous != size {
			return nil, nil, errors.New("Conflicting source object sizes")
		}
		objects[hash] = size
		if charges[id] == nil {
			charges[id] = map[string]int64{}
		}
		charges[id][hash] = size
	}
	calculated := map[int64]int64{}
	for id, files := range charges {
		for _, size := range files {
			if calculated[owners[id]] > int64(^uint64(0)>>1)-size {
				return nil, nil, errors.New("Source logical charge overflows int64")
			}
			calculated[owners[id]] += size
		}
	}
	for id, actual := range used {
		if actual != calculated[id] {
			return nil, nil, errors.New("Source logical storage charges do not match retained references")
		}
	}
	var result []content
	for hash, size := range objects {
		result = append(result, content{hash, size, filepath.Join(source.PublicDir, ".objects", hash)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].hash < result[j].hash })
	return result, received, nil
}
func regular(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("Migration requires regular files without symbolic links")
	}
	return os.Open(path)
}
func save(path string, value receipt) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".migration-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(append(data, '\n'))
	if err == nil {
		err = file.Sync()
	}
	closed := file.Close()
	if err == nil {
		err = closed
	}
	if err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func empty(records [][][]any) bool {
	for _, rows := range records {
		if len(rows) != 0 {
			return false
		}
	}
	return true
}
func transfer(ctx context.Context, db *sql.DB, records [][][]any) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for n, t := range tables {
		values := strings.TrimSuffix(strings.Repeat("?,", len(strings.Split(t.columns, ","))), ",")
		statement, err := tx.Prepare("INSERT INTO " + t.name + "(" + t.columns + ") VALUES(" + values + ")")
		if err != nil {
			return err
		}
		for _, row := range records[n] {
			if _, err = statement.ExecContext(ctx, row...); err != nil {
				statement.Close()
				return err
			}
		}
		statement.Close()
	}
	return tx.Commit()
}
func copyInput(ctx context.Context, file content, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	if existing, err := regular(target); err == nil {
		digest := sha256.New()
		n, err := io.Copy(digest, existing)
		existing.Close()
		if err == nil && n == file.size && hex.EncodeToString(digest.Sum(nil)) == file.hash {
			return nil
		}
		return errors.New("Received-input target conflicts with migration source")
	} else if !errors.Is(err, os.ErrNotExist) {
		if _, e := os.Lstat(target); !os.IsNotExist(e) {
			return errors.New("Unsafe received-input target")
		}
	}
	input, err := regular(file.path)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.CreateTemp(filepath.Dir(target), ".migration-input-")
	if err != nil {
		return err
	}
	defer os.Remove(output.Name())
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(output, hash), input)
	if err == nil && (n != file.size || hex.EncodeToString(hash.Sum(nil)) != file.hash) {
		err = objectstore.ErrIntegrity
	}
	if err == nil {
		err = output.Sync()
	}
	closed := output.Close()
	if err == nil {
		err = closed
	}
	if err != nil {
		return err
	}
	if err = os.Rename(output.Name(), target); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func Run(ctx context.Context, options Options) (err error) {
	source, target := options.Source, options.Target
	if source.DatabaseBackend != "sqlite" && source.DatabaseBackend != "" || options.SourceStorage.Backend != "local" {
		return errors.New("Migration source must be stopped SQLite with local storage")
	}
	if overlap(source.Root, target.Root) || overlap(source.DBDir, target.DBDir) || overlap(source.DataDir, target.DataDir) {
		return errors.New("Use separate source/target deployment, database-lock and staging directories")
	}
	if target.DatabaseBackend != "mysql" && options.TargetStorage.Backend != "s3" {
		return errors.New("Migration requires a MySQL or S3 target")
	}
	origin, version, err := store.OpenMigrationSource(source.DBDir)
	if err != nil {
		return err
	}
	defer origin.Close()
	records, err := readTables(origin.DB, version)
	if err != nil {
		return err
	}
	objects, received, err := inventory(records, source)
	if err != nil {
		return err
	}
	// Paths are excluded from the content inventory; only verified bytes bind a resumed run.
	contents := []any{}
	for _, object := range objects {
		contents = append(contents, []any{object.hash, object.size})
	}
	for _, input := range received {
		contents = append(contents, []any{filepath.Base(filepath.Dir(input.path)), input.hash, input.size})
	}
	sourceDigest := digest([]any{records, contents, origin.Namespace()})
	destination, err := store.OpenDeployment(target)
	if err != nil {
		return err
	}
	defer destination.Close()
	namespace := origin.Namespace()
	if namespace == "" {
		namespace = destination.Namespace()
	}
	path := filepath.Join(target.Root, ".pebril-migration.json")
	targetIdentity := digest([]any{target.DatabaseBackend, target.DBDir, target.DataDir, target.PublicDir, options.TargetStorage.Backend, func() any {
		if options.TargetStorage.S3 == nil {
			return nil
		}
		c := options.TargetStorage.S3
		return []string{c.Endpoint, c.Bucket, c.Region, c.Addressing, c.Prefix}
	}()})
	state := receipt{Version: 1, Source: sourceDigest, Target: targetIdentity, Namespace: namespace, Phase: "preflight", Objects: len(objects)}
	for _, rows := range records {
		state.Records += len(rows)
	}
	existed := false
	if info, e := os.Lstat(path); e == nil {
		if !info.Mode().IsRegular() {
			return errors.New("Migration receipt must be a regular private file")
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		var previous receipt
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&previous) != nil || decoder.Decode(new(any)) != io.EOF || previous.Version != 1 || previous.Source != state.Source || previous.Target != state.Target || previous.Namespace != state.Namespace {
			return errors.New("Migration receipt conflicts with source or target")
		}
		existed = true
	} else if !os.IsNotExist(e) {
		return e
	}
	writeFailure := existed
	defer func() {
		if err != nil && writeFailure {
			state.Phase = "failed"
			_ = save(path, state)
		}
	}()
	current, err := readTables(destination.DB, 4)
	if err != nil {
		return err
	}
	if !empty(current) && (!existed || digest(current) != digest(records)) {
		return errors.New("Migration target contains conflicting records")
	}
	if !existed {
		if err = save(path, state); err != nil {
			return err
		}
	}
	writeFailure = true
	if err = destination.SetMigrationNamespace(namespace); err != nil {
		return err
	}
	selected, err := objectstore.New(ctx, filepath.Join(target.PublicDir, ".objects"), options.TargetStorage, namespace)
	if err != nil {
		return err
	}
	progress := func(phase string) error {
		state.Phase = phase
		if options.Progress != nil {
			fmt.Fprintf(options.Progress, "Migration phase: %s (%d records, %d objects)\n", phase, state.Records, state.Objects)
		}
		return save(path, state)
	}
	if err = progress("copying_objects"); err != nil {
		return err
	}
	for _, object := range objects {
		file, e := regular(object.path)
		if e != nil {
			return errors.New("Source image object is missing or unsafe")
		}
		file.Close()
		hash, size, e := selected.Publish(ctx, object.path)
		if e != nil {
			return e
		}
		if hash != object.hash || size != object.size {
			return objectstore.ErrIntegrity
		}
	}
	for _, input := range received {
		id := filepath.Base(filepath.Dir(input.path))
		if err = copyInput(ctx, input, filepath.Join(target.DataDir, ".incoming", id, "input")); err != nil {
			return err
		}
	}
	if err = progress("copying_records"); err != nil {
		return err
	}
	if empty(current) {
		if err = transfer(ctx, destination.DB, records); err != nil {
			return errors.New("Record transfer failed; keep source stopped and rerun with the same roots after resolving the target failure")
		}
	}
	if err = progress("verifying"); err != nil {
		return err
	}
	copied, err := readTables(destination.DB, 4)
	if err != nil {
		return err
	}
	if digest(copied) != digest(records) {
		return errors.New("Target record inventory does not match source")
	}
	for _, object := range objects {
		file, e := selected.Open(ctx, object.hash, object.size)
		if e != nil {
			return e
		}
		hash := sha256.New()
		count, e := io.Copy(hash, io.LimitReader(file, object.size+1))
		file.Close()
		if e != nil || count != object.size || hex.EncodeToString(hash.Sum(nil)) != object.hash {
			return objectstore.ErrIntegrity
		}
	}
	if err = progress("completed"); err != nil {
		return err
	}
	return nil
}

func overlap(a, b string) bool {
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative) {
			return true
		}
	}
	return false
}
