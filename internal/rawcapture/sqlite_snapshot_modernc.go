//go:build windows

package rawcapture

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"unsafe"

	"modernc.org/libc"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const sqliteSnapshotDriverName = "sqlite"

func sqliteSnapshotConnectionIdentity(connection *sql.Conn) (string, error) {
	var identity string
	err := connection.Raw(func(driverConnection any) error {
		value := reflect.ValueOf(driverConnection)
		if value.Kind() != reflect.Pointer || value.IsNil() ||
			value.Elem().Type().PkgPath() != "modernc.org/sqlite" ||
			value.Elem().Type().Name() != "conn" {
			return fmt.Errorf("unexpected SQLite source driver %T", driverConnection)
		}
		connectionType := value.Elem().Type()
		databaseField, databaseOK := connectionType.FieldByName("db")
		tlsField, tlsOK := connectionType.FieldByName("tls")
		if !databaseOK || databaseField.Type.Kind() != reflect.Uintptr ||
			!tlsOK || tlsField.Type != reflect.TypeFor[*libc.TLS]() {
			return errors.New("rawcapture: SQLite source driver layout changed")
		}
		base := unsafe.Pointer(value.Pointer())
		database := *(*uintptr)(unsafe.Add(base, databaseField.Offset))
		tls := *(**libc.TLS)(unsafe.Add(base, tlsField.Offset))
		if database == 0 || tls == nil {
			return errors.New("rawcapture: SQLite source driver has no open database")
		}
		name := tls.Alloc(5)
		defer tls.Free(5)
		copy(unsafe.Slice((*byte)(unsafe.Pointer(name)), 5), "main\x00")
		var identityErr error
		identity, identityErr = sqliteSnapshotModerncConnectionIdentity(
			tls, database, name,
		)
		if identityErr != nil {
			return identityErr
		}
		return nil
	})
	return identity, err
}

func sqliteOnlineBackup(
	ctx context.Context, connection *sql.Conn, destinationPath string, maxBytes int64,
) error {
	var pages, pageBytes int64
	if err := connection.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return fmt.Errorf("rawcapture: read SQLite page count: %w", err)
	}
	if err := connection.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageBytes); err != nil {
		return fmt.Errorf("rawcapture: read SQLite page size: %w", err)
	}
	if err := sqliteBackupSizeError(pages, pageBytes, maxBytes); err != nil {
		return err
	}
	err := connection.Raw(func(driverConnection any) (resultErr error) {
		backuper, ok := driverConnection.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("unexpected SQLite source driver %T", driverConnection)
		}
		backup, err := backuper.NewBackup(sqliteSnapshotDSN(destinationPath, false))
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, backup.Finish()) }()
		return runSQLiteBackup(
			ctx, pageBytes, maxBytes,
			func() (bool, error) {
				more, err := backup.Step(128)
				if sqliteModerncBackupBusy(err) {
					return false, nil
				}
				return !more, err
			},
			backup.Remaining,
			backup.PageCount,
		)
	})
	if err != nil {
		return fmt.Errorf("rawcapture: back up SQLite source: %w", err)
	}
	return nil
}

func sqliteModerncBackupBusy(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	code := sqliteErr.Code() & 0xff
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}

func sqliteSnapshotDSN(path string, readOnly bool) string {
	values := url.Values{}
	values.Add("_pragma", "busy_timeout(5000)")
	if readOnly {
		values.Set("mode", "ro")
	} else {
		values.Set("mode", "rwc")
	}
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + values.Encode()
}
