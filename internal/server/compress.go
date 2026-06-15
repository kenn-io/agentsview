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
	if r.URL.Path == "/api/v1/events" || strings.HasSuffix(r.URL.Path, "/watch") {
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
	if !canGzipStatus(w.status) || w.Header().Get("Content-Encoding") != "" {
		w.writePlainHeader()
		_, err := w.ResponseWriter.Write(p)
		return len(p), err
	}
	if w.compressing {
		_, err := w.writer.Write(p)
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
		return
	}
	w.ResponseWriter.WriteHeader(w.status)
	w.sentHeader = true
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
