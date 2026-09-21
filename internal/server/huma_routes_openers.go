package server

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
)

func (s *Server) registerOpenersRoutes() {
	group := huma.NewGroup(s.api, "/api/v1/openers")
	configureRouteGroup(group, "Openers")

	s.get(group, "", "List openers", s.humaListOpeners)
}

type openersResponse struct {
	Openers []Opener `json:"openers"`
}

func (s *Server) humaListOpeners(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[openersResponse], error) {
	openers := detectOpeners()
	if openers == nil {
		openers = []Opener{}
	}
	return &jsonOutput[openersResponse]{Body: openersResponse{Openers: openers}}, nil
}
