package requestsign

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const replayApplicationID = 0x41565347

const replaySchema = `PRAGMA application_id=1096176455;
CREATE TABLE signing_state (version INTEGER NOT NULL, highwater INTEGER NOT NULL);
INSERT INTO signing_state VALUES (1, 0);
CREATE TABLE signing_nonces (key_id TEXT NOT NULL, nonce_hash BLOB NOT NULL, expires INTEGER NOT NULL, PRIMARY KEY(key_id,nonce_hash));
CREATE INDEX signing_expiry ON signing_nonces(expires);`

type Replay struct {
	db    *sql.DB
	limit int
}

// InitReplay creates new state exclusively. Recovery must rotate all accepted
// keys before initializing a new file; it cannot overwrite existing state.
func InitReplay(path string) error {
	f, err := createPrivateFile(path)
	if err != nil {
		return errors.New("creating signing replay state failed; existing state must be retained")
	}
	if err = ensurePrivateFile(f); err != nil {
		_ = f.Close()
		return errors.New("signing files must be private regular files")
	}
	if err = f.Close(); err != nil {
		return err
	}
	db, err := openReplayDB(path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.ExecContext(context.Background(), replaySchema); err != nil {
		return errors.New("initializing signing replay state failed")
	}
	// EXTRA commits sync the database and journal directory where supported.
	if err = syncReplayDirectory(filepath.Dir(path)); err != nil {
		return errors.New("persisting signing replay directory failed")
	}
	return nil
}

func openReplayDB(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, ErrReplay
	}
	db, err := sql.Open("sqlite3", replayDSN(abs, "rw"))
	if err != nil {
		return nil, ErrReplay
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

func OpenReplay(path string) (*Replay, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("signing replay state is missing or not private; initialize explicitly")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("signing replay state is missing or not private; initialize explicitly")
	}
	private := isPrivateRegularFile(file)
	_ = file.Close()
	if !private {
		return nil, errors.New("signing replay state is missing or not private; initialize explicitly")
	}
	// Check identity without write pragmas first. A mistaken archive path must
	// never change that database's journal mode or other persistent settings.
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, ErrReplay
	}
	probe, err := sql.Open("sqlite3", replayDSN(abs, "ro"))
	if err != nil {
		return nil, ErrReplay
	}
	var applicationID int
	err = probe.QueryRowContext(context.Background(), "PRAGMA application_id").Scan(&applicationID)
	_ = probe.Close()
	if err != nil || applicationID != replayApplicationID {
		return nil, ErrReplay
	}
	db, err := openReplayDB(path)
	if err != nil {
		return nil, err
	}
	fail := func() (*Replay, error) { _ = db.Close(); return nil, ErrReplay }
	var version, count int
	var highwater int64
	if db.QueryRowContext(context.Background(), "SELECT version, highwater FROM signing_state").Scan(&version, &highwater) != nil || version != 1 || highwater < 0 {
		return fail()
	}
	if db.QueryRowContext(context.Background(), "SELECT count(*) FROM signing_state").Scan(&count) != nil || count != 1 {
		return fail()
	}
	if db.QueryRowContext(context.Background(), "SELECT count(*) FROM signing_nonces").Scan(&count) != nil || count > 100000 {
		return fail()
	}
	var integrity string
	if db.QueryRowContext(context.Background(), "PRAGMA quick_check").Scan(&integrity) != nil || integrity != "ok" {
		return fail()
	}
	return &Replay{db: db, limit: 100000}, nil
}

func replayDSN(absPath, mode string) string {
	return replayDSNForPath(absPath, filepath.VolumeName(absPath), mode)
}

func replayDSNForPath(absPath, volume, mode string) string {
	path := replayURIPath(absPath, volume)
	u := url.URL{Scheme: "file", Path: path}
	query := url.Values{
		"mode":          {mode},
		"_busy_timeout": {"1000"},
	}
	if mode == "rw" {
		query.Set("_txlock", "immediate")
		query.Set("_synchronous", "EXTRA")
		query.Set("_journal_mode", "DELETE")
	}
	u.RawQuery = query.Encode()
	return u.String()
}

func replayURIPath(absPath, volume string) string {
	path := filepath.ToSlash(absPath)
	if len(volume) == 2 && volume[1] == ':' {
		path = "/" + path
	}
	return path
}

func (s *Replay) Close() error { return s.db.Close() }

func (s *Replay) Admit(ctx context.Context, keyID, nonce string, created, expires int64) error {
	return s.admit(ctx, keyID, nonce, created, expires, nil)
}

func (s *Replay) admit(ctx context.Context, keyID, nonce string, created, expires int64, check func() error) error {
	// _txlock=immediate acquires the database writer lock before sampling time.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrReplay
	}
	defer func() { _ = tx.Rollback() }()
	if check != nil {
		if err = check(); err != nil {
			return err
		}
	}
	now := time.Now()
	if !fresh(created, expires, now) {
		return ErrInvalid
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	var highwater int64
	if err = tx.QueryRowContext(ctx, "SELECT highwater FROM signing_state WHERE version=1").Scan(&highwater); err != nil || now.Unix() < highwater {
		return ErrReplay
	}
	// Expiry is exclusive, so an entry at the current second is no longer usable.
	if _, err = tx.ExecContext(ctx, "DELETE FROM signing_nonces WHERE expires <= ?", now.Unix()); err != nil {
		return ErrReplay
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM signing_nonces").Scan(&count); err != nil || count >= s.limit {
		return ErrReplay
	}
	hash := sha256.Sum256([]byte(nonce))
	if _, err = tx.ExecContext(ctx, "INSERT INTO signing_nonces(key_id,nonce_hash,expires) VALUES(?,?,?)", keyID, hash[:], expires); err != nil {
		return ErrReplay
	}
	result, err := tx.ExecContext(ctx, "UPDATE signing_state SET highwater = ? WHERE version=1", now.Unix())
	if err != nil {
		return ErrReplay
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return ErrReplay
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("%w: durable admission failed", ErrReplay)
	}
	return nil
}
