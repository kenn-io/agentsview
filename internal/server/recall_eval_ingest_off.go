//go:build !evalingest

package server

// registerEvalIngestRoutes is a no-op without the evalingest build tag: the
// raw-trajectory ingest endpoint is a lab-only surface and the default
// binary does not serve it.
func (s *Server) registerEvalIngestRoutes() {}
