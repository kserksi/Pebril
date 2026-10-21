package store

import (
	"database/sql"
	"errors"
)

// Tombstones retain the last generation so a late DELETE can never target a newly published object.
func objectRow(tx *sql.Tx, hash string) (int64, sql.NullInt64, error) {
	var generation int64
	var active sql.NullInt64
	err := tx.QueryRow("SELECT generation,active_generation FROM object_keys WHERE hash=?", hash).Scan(&generation, &active)
	return generation, active, err
}
func objectReferences(tx *sql.Tx, hash string) (bool, error) {
	var count int
	err := tx.QueryRow("SELECT (SELECT COUNT(*) FROM images WHERE hash=?)+(SELECT COUNT(*) FROM image_files WHERE hash=?)", hash, hash).Scan(&count)
	return count > 0, err
}
func (s *Store) AcquireObjectKey(hash string) (int64, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	generation, active, err := objectRow(tx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		referenced, e := objectReferences(tx, hash)
		if e != nil {
			return 0, e
		}
		generation = 1
		if referenced {
			generation = 0
		}
		if _, err = tx.Exec("INSERT INTO object_keys(hash,generation,active_generation) VALUES(?,?,?)", hash, generation, generation); err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	} else if active.Valid {
		return active.Int64, tx.Commit()
	} else {
		if generation == int64(^uint64(0)>>1) {
			return 0, errors.New("Object generation exhausted")
		}
		generation++
		if _, err = tx.Exec("UPDATE object_keys SET generation=?,active_generation=? WHERE hash=?", generation, generation, hash); err != nil {
			return 0, err
		}
	}
	return generation, tx.Commit()
}
func (s *Store) ResolveObjectKey(hash string) (int64, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	_, active, err := objectRow(tx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		referenced, e := objectReferences(tx, hash)
		if e != nil {
			return 0, e
		}
		if !referenced {
			return 0, sql.ErrNoRows
		}
		// Owned hash-only objects from the earlier development layout remain readable until retirement.
		if _, err = tx.Exec("INSERT INTO object_keys(hash,generation,active_generation) VALUES(?,0,0)", hash); err != nil {
			return 0, err
		}
		return 0, tx.Commit()
	}
	if err != nil {
		return 0, err
	}
	if !active.Valid {
		return 0, sql.ErrNoRows
	}
	return active.Int64, tx.Commit()
}
func (s *Store) RetireObjectKey(hash string, generation int64) (bool, error) {
	if generation < 0 {
		return false, errors.New("Invalid object generation")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	last, active, err := objectRow(tx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		referenced, e := objectReferences(tx, hash)
		if e != nil {
			return false, e
		}
		if referenced {
			active = sql.NullInt64{Valid: true, Int64: 0}
		}
		last = generation
		var value any
		if active.Valid {
			value = active.Int64
		}
		if _, err = tx.Exec("INSERT INTO object_keys(hash,generation,active_generation) VALUES(?,?,?)", hash, last, value); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	} else if generation > last {
		if _, err = tx.Exec("UPDATE object_keys SET generation=? WHERE hash=?", generation, hash); err != nil {
			return false, err
		}
	}
	if active.Valid && active.Int64 == generation {
		referenced, e := objectReferences(tx, hash)
		if e != nil {
			return false, e
		}
		if referenced {
			return false, tx.Commit()
		}
		if _, err = tx.Exec("UPDATE object_keys SET active_generation=NULL WHERE hash=? AND active_generation=?", hash, generation); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func (s *Store) BindStorage(backend string) error {
	if backend != "local" && backend != "s3" {
		return errors.New("Invalid storage backend")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previous string
	if err = tx.QueryRow("SELECT storage_backend FROM deployment WHERE id=1").Scan(&previous); err != nil {
		return err
	}
	if previous == backend {
		return tx.Commit()
	}
	if !(previous == "" && backend == "local") {
		var count int
		if err = tx.QueryRow("SELECT COUNT(*) FROM images").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return errors.New("Storage backend change requires a stopped, verified migration; changing configuration does not move existing data")
		}
	}
	if _, err = tx.Exec("UPDATE deployment SET storage_backend=? WHERE id=1", backend); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) CanInitializeObjectNamespace() (bool, error) {
	var count int
	err := s.DB.QueryRow("SELECT COUNT(*) FROM images").Scan(&count)
	return count == 0, err
}
