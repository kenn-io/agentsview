package kept

import (
	"bytes"
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
	reports           Reports
	mu                sync.Mutex
	maxFile, maxBytes int64
	maxEntries        int
}

func (d *Disk) Open(subdir, id string, maxFile, maxBytes int64, maxEntries int) error {
	if err := d.reports.OpenDisk(subdir, id); err != nil {
		return err
	}
	d.reports.disk.suffix = ".gob"
	d.maxFile, d.maxBytes, d.maxEntries = maxFile, maxBytes, maxEntries
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.trim()
}

func (d *Disk) Enabled() bool { return d.reports.disk.dir != "" }

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
	path := disk.path(selection)
	info, err := os.Stat(path)
	if err != nil || (d.maxFile > 0 && info.Size() > d.maxFile) {
		return nil, false
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	var reader io.Reader = file
	if d.maxFile > 0 {
		reader = io.LimitReader(file, d.maxFile+1)
	}
	data, err := io.ReadAll(reader)
	closeErr := file.Close()
	if err != nil || closeErr != nil || (d.maxFile > 0 && int64(len(data)) > d.maxFile) {
		return nil, false
	}
	header := disk.header(version)
	if !bytes.HasPrefix(data, header) {
		return nil, false
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	return data[len(header):], true
}

// Touch records a read served by the in-memory memo.
func (d *Disk) Touch(selection string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reports.disk.dir == "" || selection == "" {
		return
	}
	now := time.Now()
	_ = os.Chtimes(d.reports.disk.path(selection), now, now)
}

func (d *Disk) Put(selection, version string, payload []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	disk := d.reports.disk
	if disk.dir == "" {
		return
	}
	data := append(disk.header(version), payload...)
	if d.maxFile > 0 && int64(len(data)) > d.maxFile {
		return
	}
	if writeFileAtomic(disk.path(selection), data) == nil {
		_ = d.trim()
	}
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
	for _, f := range files {
		if (d.maxBytes <= 0 || total <= d.maxBytes) && (d.maxEntries <= 0 || len(files) <= d.maxEntries) {
			break
		}
		if err := os.Remove(filepath.Join(d.reports.disk.dir, f.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing kept file: %w", err)
		}
		total -= f.size
		files = files[1:]
	}
	return nil
}
