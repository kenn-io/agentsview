package parser

import "strings"

func ProviderFindRequestWithRawSessionID(
	def AgentDef,
	req FindSourceRequest,
) FindSourceRequest {
	if req.RawSessionID != "" {
		req.RawSessionID = ProviderNormalizeRawSessionID(def, req.RawSessionID)
		return req
	}
	req.RawSessionID = ProviderRawSessionIDFromFull(def, req.FullSessionID)
	return req
}

func ProviderNormalizeRawSessionID(def AgentDef, id string) string {
	_, id = StripHostPrefix(id)
	if def.IDPrefix != "" && strings.HasPrefix(id, def.IDPrefix) {
		return strings.TrimPrefix(id, def.IDPrefix)
	}
	return id
}

func ProviderRawSessionIDFromFull(def AgentDef, id string) string {
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
