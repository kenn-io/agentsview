//go:build !windows && cgo

package rawcapture

/*
typedef struct sqlite3 sqlite3;
typedef struct sqlite3_file sqlite3_file;
int sqlite3_file_control(sqlite3*, const char*, int, void*);
const char *sqlite3_sourceid(void);
void sqlite3_free(void*);

#include <sys/stat.h>
#include <string.h>

#define RAWCAPTURE_SQLITE_FCNTL_FILE_POINTER 7
#define RAWCAPTURE_SQLITE_FCNTL_VFSNAME 12
#define RAWCAPTURE_SQLITE_SOURCE_ID \
	"2026-07-24 19:02:57 bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc"

typedef struct rawcaptureUnixFile {
	const void *methods;
	void *vfs;
	void *inode;
	int fd;
} rawcaptureUnixFile;

static int rawcaptureSQLiteIdentity(
	void *db, unsigned long long *device, unsigned long long *inode
) {
	// The unixFile layout is private SQLite ABI. Fail closed unless both the
	// bundled amalgamation and one of its Unix VFS implementations are active.
	if (strcmp(sqlite3_sourceid(), RAWCAPTURE_SQLITE_SOURCE_ID) != 0) return -2;
	char *vfsName = 0;
	int result = sqlite3_file_control(
		(sqlite3*)db, "main", RAWCAPTURE_SQLITE_FCNTL_VFSNAME, &vfsName
	);
	if (result != 0 || vfsName == 0) return result == 0 ? -3 : result;
	int supportedVFS = strncmp(vfsName, "unix", 4) == 0
		&& (vfsName[4] == '\0' || vfsName[4] == '-');
	sqlite3_free(vfsName);
	if (!supportedVFS) return -4;

	sqlite3_file *file = 0;
	result = sqlite3_file_control(
		(sqlite3*)db, "main", RAWCAPTURE_SQLITE_FCNTL_FILE_POINTER, &file
	);
	if (result != 0 || file == 0) return result == 0 ? -1 : result;
	struct stat info;
	if (fstat(((rawcaptureUnixFile*)file)->fd, &info) != 0) return -1;
	*device = (unsigned long long)info.st_dev;
	*inode = (unsigned long long)info.st_ino;
	return 0;
}
*/
import "C"

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"unsafe"

	"github.com/mattn/go-sqlite3"
)

const sqliteSnapshotDriverName = "sqlite3"

func sqliteSnapshotConnectionIdentity(connection *sql.Conn) (string, error) {
	var device, inode C.ulonglong
	err := connection.Raw(func(driverConnection any) error {
		sqliteConnection, ok := driverConnection.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("unexpected SQLite source driver %T", driverConnection)
		}
		field, ok := reflect.TypeOf(sqliteConnection).Elem().FieldByName("db")
		if !ok {
			return errors.New("rawcapture: SQLite source driver has no database handle")
		}
		if field.Type.Kind() != reflect.Pointer && field.Type.Kind() != reflect.UnsafePointer {
			return errors.New("rawcapture: SQLite source driver database handle changed type")
		}
		database := *(*unsafe.Pointer)(unsafe.Add(unsafe.Pointer(sqliteConnection), field.Offset))
		if database == nil {
			return errors.New("rawcapture: SQLite source driver has no open database")
		}
		if result := C.rawcaptureSQLiteIdentity(database, &device, &inode); result != 0 {
			return fmt.Errorf("rawcapture: inspect SQLite source identity: result %d", result)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", uint64(device), uint64(inode)), nil
}

func sqliteOnlineBackup(
	ctx context.Context, sourceConn *sql.Conn, destinationPath string, maxBytes int64,
) error {
	destination, err := sql.Open("sqlite3", sqliteSnapshotDSN(destinationPath, false))
	if err != nil {
		return fmt.Errorf("rawcapture: open SQLite snapshot: %w", err)
	}
	defer destination.Close()
	destinationConn, err := destination.Conn(ctx)
	if err != nil {
		return fmt.Errorf("rawcapture: connect SQLite snapshot: %w", err)
	}
	defer destinationConn.Close()
	var pages, pageBytes int64
	if err := sourceConn.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return fmt.Errorf("rawcapture: read SQLite page count: %w", err)
	}
	if err := sourceConn.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageBytes); err != nil {
		return fmt.Errorf("rawcapture: read SQLite page size: %w", err)
	}
	if err := sqliteBackupSizeError(pages, pageBytes, maxBytes); err != nil {
		return err
	}

	err = sourceConn.Raw(func(sourceDriver any) error {
		sourceSQLite, ok := sourceDriver.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("unexpected SQLite source driver %T", sourceDriver)
		}
		return destinationConn.Raw(func(destinationDriver any) (resultErr error) {
			destinationSQLite, ok := destinationDriver.(*sqlite3.SQLiteConn)
			if !ok {
				return fmt.Errorf("unexpected SQLite snapshot driver %T", destinationDriver)
			}
			backup, err := destinationSQLite.Backup("main", sourceSQLite, "main")
			if err != nil {
				return err
			}
			defer func() { resultErr = errors.Join(resultErr, backup.Finish()) }()
			return runSQLiteBackup(
				ctx, pageBytes, maxBytes,
				func() (bool, error) { return backup.Step(128) },
				backup.Remaining,
				backup.PageCount,
			)
		})
	})
	if err != nil {
		return fmt.Errorf("rawcapture: back up SQLite source: %w", err)
	}
	return nil
}

func sqliteSnapshotDSN(path string, readOnly bool) string {
	values := url.Values{}
	values.Set("_busy_timeout", "5000")
	if readOnly {
		values.Set("mode", "ro")
	} else {
		values.Set("mode", "rwc")
	}
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + values.Encode()
}
