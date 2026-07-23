package server

import (
	"compress/gzip"
	"net/http"
	"strings"
)

const gzipMinLength = 512

func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !shouldGzip(r) {
			next.ServeHTTP(w, r)
			return
		}

		gw := &gzipResponseWriter{
			ResponseWriter: w,
			minLength:      gzipMinLength,
		}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

func shouldGzip(r *http.Request) bool {
	if r.Method == http.MethodHead {
		return false
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	if r.Header.Get("Range") != "" {
		return false
	}
	if !headerContainsToken(r.Header.Get("Accept-Encoding"), "gzip") {
		return false
	}
	if headerContainsToken(r.Header.Get("Accept"), "text/event-stream") {
		return false
	}
	if isStreamingPath(r.URL.Path) {
		return false
	}
	return true
}

type gzipResponseWriter struct {
	http.ResponseWriter
	writer      *gzip.Writer
	minLength   int
	status      int
	buffer      []byte
	wroteHeader bool
	sentHeader  bool
	compressing bool
	plain       bool
}

func (w *gzipResponseWriter) Header() http.Header {
	return w.ResponseWriter.Header()
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	if !canGzipStatus(status) || w.Header().Get("Content-Encoding") != "" {
		w.ResponseWriter.WriteHeader(status)
		w.sentHeader = true
		return
	}
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.compressing {
		_, err := w.writer.Write(p)
		return len(p), err
	}
	if w.plain || !canGzipStatus(w.status) ||
		w.Header().Get("Content-Encoding") != "" ||
		isEventStreamContentType(w.Header().Get("Content-Type")) {
		w.writePlainHeader()
		_, err := w.ResponseWriter.Write(p)
		return len(p), err
	}

	w.buffer = append(w.buffer, p...)
	if len(w.buffer) < w.minLength {
		return len(p), nil
	}
	if err := w.startGzip(); err != nil {
		return 0, err
	}
	_, err := w.writer.Write(w.buffer)
	w.buffer = nil
	return len(p), err
}

func (w *gzipResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.compressing {
		_ = w.writer.Flush()
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	if len(w.buffer) > 0 {
		w.writePlainHeader()
		_, _ = w.ResponseWriter.Write(w.buffer)
		w.buffer = nil
	} else if !w.sentHeader {
		w.writePlainHeader()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *gzipResponseWriter) close() {
	if !w.wroteHeader {
		return
	}
	if w.compressing {
		_ = w.writer.Close()
		return
	}
	if len(w.buffer) > 0 {
		w.writePlainHeader()
		_, _ = w.ResponseWriter.Write(w.buffer)
		w.buffer = nil
	} else if !w.sentHeader {
		w.writePlainHeader()
	}
}

func (w *gzipResponseWriter) startGzip() error {
	if w.plain || w.sentHeader ||
		isEventStreamContentType(w.Header().Get("Content-Type")) {
		return nil
	}
	if w.Header().Get("Content-Type") == "" && len(w.buffer) > 0 {
		w.Header().Set("Content-Type", http.DetectContentType(w.buffer))
	}
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Encoding", "gzip")
	addVaryHeader(w.Header(), "Accept-Encoding")
	w.writer = gzip.NewWriter(w.ResponseWriter)
	w.compressing = true
	w.ResponseWriter.WriteHeader(w.status)
	w.sentHeader = true
	return nil
}

func (w *gzipResponseWriter) writePlainHeader() {
	if w.sentHeader {
		w.plain = true
		return
	}
	w.ResponseWriter.WriteHeader(w.status)
	w.sentHeader = true
	w.plain = true
}

func canGzipStatus(status int) bool {
	return status != http.StatusNoContent &&
		status != http.StatusNotModified &&
		status >= 200
}

func headerContainsToken(value, token string) bool {
	for part := range strings.SplitSeq(value, ",") {
		if strings.EqualFold(strings.TrimSpace(strings.Split(part, ";")[0]), token) {
			return true
		}
	}
	return false
}

func addVaryHeader(h http.Header, value string) {
	vary := h.Values("Vary")
	for _, line := range vary {
		if headerContainsToken(line, value) {
			return
		}
	}
	h.Add("Vary", value)
}

func isEventStreamContentType(value string) bool {
	mediaType, _, _ := strings.Cut(value, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "text/event-stream")
}

func isStreamingPath(path string) bool {
	switch {
	case path == "/api/v1/events",
		path == "/api/v1/sync",
		path == "/api/v1/resync",
		path == "/api/v1/secrets/scan",
		path == "/api/v1/insights/generate":
		return true
	case strings.HasSuffix(path, "/watch"):
		return true
	case strings.HasPrefix(path, "/api/v1/import/"):
		return true
	case strings.HasPrefix(path, "/api/v1/artifacts/"):
		return true
	default:
		return false
	}
}
