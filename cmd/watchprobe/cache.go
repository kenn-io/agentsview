package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"

	sqlite "modernc.org/sqlite"
)

type cache struct {
	db   *sql.DB
	path string
}

const cacheID = 0x41565054

func openCache(ctx context.Context, path string, maxBytes int64) (*cache, error) {
	if maxBytes < 128<<10 {
		return nil, errors.New("cache main-file limit must be at least 128 KiB")
	}
	fresh := false
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if e != nil {
			return nil, e
		}
		if e = f.Close(); e != nil {
			return nil, e
		}
		fresh = true
	} else if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	success := false
	defer func() {
		if !success {
			db.Close()
		}
	}()
	if !fresh {
		var id int
		if err = db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&id); err != nil {
			return nil, err
		}
		if id != cacheID {
			return nil, errors.New("unrecognized tester cache")
		}
	}
	if fresh {
		if _, err = db.ExecContext(ctx, fmt.Sprintf(`PRAGMA page_size=4096; PRAGMA auto_vacuum=INCREMENTAL; PRAGMA application_id=%d;`, cacheID)); err != nil {
			return nil, err
		}
	}
	_, err = db.ExecContext(ctx, fmt.Sprintf(`PRAGMA journal_mode=TRUNCATE; PRAGMA cache_size=-8192; PRAGMA temp_store=MEMORY; PRAGMA max_page_count=%d;
 CREATE TABLE IF NOT EXISTS files(unit INTEGER NOT NULL,name TEXT NOT NULL,size INTEGER,mtime INTEGER,ctime INTEGER,volume BLOB,fileid BLOB,known INTEGER,PRIMARY KEY(unit,name)) WITHOUT ROWID;`, maxBytes/4096))
	if err != nil {
		return nil, err
	}
	success = true
	return &cache{db: db, path: path}, nil
}
func (c *cache) close() error { return c.db.Close() }
func (c *cache) load(ctx context.Context, unit int, names []string) (map[string]signature, error) {
	result := make(map[string]signature, len(names))
	if len(names) == 0 {
		return result, nil
	}
	if len(names) > pageRecords {
		return nil, errors.New("cache lookup exceeds page bound")
	}
	args := make([]any, len(names)+1)
	args[0] = unit
	for i, name := range names {
		args[i+1] = name
	}
	query := "SELECT name,size,mtime,ctime,volume,fileid,known FROM files WHERE unit=? AND name IN (" + strings.TrimSuffix(strings.Repeat("?,", len(names)), ",") + ")"
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var s signature
		var volume, id []byte
		var known int
		if err = rows.Scan(&name, &s.Size, &s.MtimeNS, &s.ChangeNS, &volume, &id, &known); err != nil {
			return nil, err
		}
		if len(volume) != 8 || len(id) != 16 {
			return nil, errors.New("invalid cache identity representation")
		}
		s.Volume = binary.LittleEndian.Uint64(volume)
		copy(s.Identity[:], id)
		s.IdentityKnown = known&1 != 0
		s.ChangeKnown = known&2 != 0
		result[name] = s
	}
	return result, rows.Err()
}
func (c *cache) write(ctx context.Context, unit int, records []record) (bool, error) {
	if len(records) > pageRecords {
		return false, errors.New("cache update exceeds page bound")
	}
	if len(records) == 0 {
		return true, nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, "INSERT OR REPLACE INTO files VALUES(?,?,?,?,?,?,?,?)")
	if err != nil {
		return false, err
	}
	defer stmt.Close()
	for _, r := range records {
		var volume [8]byte
		binary.LittleEndian.PutUint64(volume[:], r.Signature.Volume)
		known := 0
		if r.Signature.IdentityKnown {
			known |= 1
		}
		if r.Signature.ChangeKnown {
			known |= 2
		}
		_, err = stmt.ExecContext(ctx, unit, r.Name, r.Signature.Size, r.Signature.MtimeNS, r.Signature.ChangeNS, volume[:], r.Signature.Identity[:], known)
		if err != nil {
			if cacheFull(err) {
				return false, nil
			}
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		if cacheFull(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
func cacheFull(err error) bool {
	e, ok := errors.AsType[*sqlite.Error](err)
	return ok && e.Code()&255 == 13
}
func (c *cache) remove(ctx context.Context, unit int, name string) error {
	_, err := c.db.ExecContext(ctx, "DELETE FROM files WHERE unit=? AND name=?", unit, name)
	return err
}
