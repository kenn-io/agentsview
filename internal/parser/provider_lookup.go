package parser

import "strings"

func providerFindRequestWithRawSessionID(
	def AgentDef,
	req FindSourceRequest,
) FindSourceRequest {
	if req.RawSessionID != "" {
		req.RawSessionID = providerNormalizeRawSessionID(def, req.RawSessionID)
		return req
	}
	req.RawSessionID = providerRawSessionIDFromFull(def, req.FullSessionID)
	return req
}

func providerNormalizeRawSessionID(def AgentDef, id string) string {
	_, id = StripHostPrefix(id)
	if def.IDPrefix != "" && strings.HasPrefix(id, def.IDPrefix) {
		return strings.TrimPrefix(id, def.IDPrefix)
	}
	return id
}

func providerRawSessionIDFromFull(def AgentDef, id string) string {
	if id == "" {
		return ""
	}
	_, rawID := StripHostPrefix(id)
	if def.IDPrefix == "" {
		return rawID
	}
	if !strings.HasPrefix(rawID, def.IDPrefix) {
		return ""
	}
	return strings.TrimPrefix(rawID, def.IDPrefix)
}
