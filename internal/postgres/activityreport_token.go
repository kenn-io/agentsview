package postgres

import "go.kenn.io/agentsview/internal/db"

func (s *Store) EncodeActivityReportToken(payload []byte) (string, error) {
	s.runtimeState().cursorMu.RLock()
	secret := append([]byte(nil), s.runtimeState().cursorSecret...)
	s.runtimeState().cursorMu.RUnlock()
	return db.EncodeSignedActivityReportToken(secret, payload)
}

func (s *Store) DecodeActivityReportToken(token string) ([]byte, error) {
	s.runtimeState().cursorMu.RLock()
	secret := append([]byte(nil), s.runtimeState().cursorSecret...)
	s.runtimeState().cursorMu.RUnlock()
	return db.DecodeSignedActivityReportToken(secret, token)
}
