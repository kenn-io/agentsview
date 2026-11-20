package kept

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Disk retains opaque selections using the same directory and binary identity as reports.
type Disk struct {
	// MaxFiles caps retained files; zero means no limit.
	MaxFiles          int
	reports           Reports
	mu                sync.Mutex
	maxFile, maxBytes int64
}

func (d *Disk) Open(subdir, id string, maxFile, maxBytes int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.reports.OpenDisk(subdir, id); err != nil {
		return err
	}
	d.reports.disk.suffix = ".gob"
	d.maxFile, d.maxBytes = maxFile, maxBytes
	return d.trim()
}

func (d *Disk) Enabled() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reports.disk.dir != ""
}

func (d *Disk) Sweep(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reports.disk.dir == "" {
		return
	}
	_, _ = d.reports.disk.sweep(now)
	_ = d.trim()
}

func (d *Disk) Get(selection, version string) ([]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	disk := d.reports.disk
	if disk.dir == "" {
		return nil, false
	}
	data, ok, err := disk.load(selection, version, d.maxFile)
	if err != nil || !ok {
		return nil, false
	}
	_ = disk.touch(selection)
	return data, true
}

func (d activityReportDisk) load(selection, key string, maxFile int64) ([]byte, bool, error) {
	file, err := os.Open(d.path(selection))
	if err != nil {
		return nil, false, err
	}
	var reader io.Reader = file
	if maxFile > 0 {
		reader = io.LimitReader(file, maxFile+1)
	}
	data, err := io.ReadAll(reader)
	closeErr := file.Close()
	if err != nil {
		return nil, false, err
	}
	if closeErr != nil {
		return nil, false, closeErr
	}
	if maxFile > 0 && int64(len(data)) > maxFile {
		return nil, false, nil
	}
	header := d.header(key)
	if !bytes.HasPrefix(data, header) {
		return nil, false, nil
	}
	return data[len(header):], true, nil
}

func (d activityReportDisk) touch(selection string) error {
	now := time.Now()
	return os.Chtimes(d.path(selection), now, now)
}

func (d activityReportDisk) save(selection, key string, payload []byte, maxFile int64) (bool, error) {
	data := append(d.header(key), payload...)
	if maxFile > 0 && int64(len(data)) > maxFile {
		return false, nil
	}
	err := writeFileAtomic(d.path(selection), data)
	return err == nil, err
}

func (d activityReportDisk) keep(selection, version string, maxFile int64, encode func() ([]byte, error)) (bool, error) {
	err := d.touch(selection)
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	body, err := encode()
	if err != nil {
		return false, err
	}
	_, err = d.save(selection, version, body, maxFile)
	return true, err
}

// Keep touches an in-memory selection and restores it if its file was removed.
func (d *Disk) Keep(selection, version string, encode func() ([]byte, error)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reports.disk.dir == "" || selection == "" {
		return
	}
	if saved, err := d.reports.disk.keep(selection, version, d.maxFile, encode); saved && err == nil {
		_ = d.trim()
	}
}

// Put reports whether the file was written within the per-file limit.
func (d *Disk) Put(selection, version string, payload []byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	disk := d.reports.disk
	if disk.dir == "" {
		return false
	}
	if saved, _ := disk.save(selection, version, payload, d.maxFile); saved {
		_ = d.trim()
		return true
	}
	return false
}

func (d *Disk) trim() error {
	entries, err := os.ReadDir(d.reports.disk.dir)
	if err != nil {
		return err
	}
	type file struct {
		name string
		size int64
		mod  time.Time
	}
	var files []file
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), d.reports.disk.fileSuffix()) {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		files = append(files, file{entry.Name(), info.Size(), info.ModTime()})
		total += info.Size()
	}
	slices.SortFunc(files, func(a, b file) int { return a.mod.Compare(b.mod) })
	for i, f := range files {
		if (d.maxBytes <= 0 || total <= d.maxBytes) && (d.MaxFiles <= 0 || len(files)-i <= d.MaxFiles) {
			break
		}
		if err := os.Remove(filepath.Join(d.reports.disk.dir, f.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing kept file: %w", err)
		}
		total -= f.size
	}
	return nil
}

// LoadJSON returns a saved value or loads and saves it for this version.
func LoadJSON[T any](d *Disk, selection, version string, current func() bool, load func() (T, error)) (T, bool, error) {
	if data, ok := d.Get(selection, version); ok {
		var value T
		if json.Unmarshal(data, &value) == nil {
			return value, current == nil || current(), nil
		}
	}
	value, err := load()
	if err != nil {
		return value, false, err
	}
	retain := current == nil || current()
	if retain && d.Enabled() {
		if data, encodeErr := json.Marshal(value, json.FormatNilSliceAsNull(true)); encodeErr == nil {
			d.Put(selection, version, data)
		}
	}
	return value, retain, nil
}
