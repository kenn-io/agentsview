package server

import (
	"archive/zip"
	"io/fs"
	"log"
	"net/http"
	"strings"
)

func (s *Server) handleChromeExtension(w http.ResponseWriter, r *http.Request) {
	if info, err := fs.Stat(s.spaFS, "chrome-extension"); err != nil || !info.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="chrome-extension.zip"`)
	archive := zip.NewWriter(w)
	err := fs.WalkDir(s.spaFS, "chrome-extension", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(s.spaFS, path)
		if err != nil {
			return err
		}
		file, err := archive.Create(strings.TrimPrefix(path, "chrome-extension/"))
		if err != nil {
			return err
		}
		_, err = file.Write(data)
		return err
	})
	closeErr := archive.Close()
	if err != nil || closeErr != nil {
		log.Printf("chrome extension download: %v, %v", err, closeErr)
	}
}
