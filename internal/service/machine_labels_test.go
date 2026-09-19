package service_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

type machineLabelsStore struct {
	db.Store
	labels service.MachineLabelCatalog
	calls  int
}

func (s *machineLabelsStore) GetMachineLabels(
	context.Context,
) (map[string]string, error) {
	s.calls++
	return s.labels, nil
}

func TestMachineLabelsFromStoreBackend(t *testing.T) {
	store := &machineLabelsStore{
		labels: service.MachineLabelCatalog{"machine-key": "Build Host"},
	}

	got, err := service.MachineLabels(
		t.Context(), service.NewReadOnlyBackend(store),
	)

	require.NoError(t, err)
	assert.Equal(t, store.labels, got)
	assert.Equal(t, 1, store.calls)
}

func TestMachineLabelsFromHTTPBackend(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w,
			`{"machines":["machine-key"],"machine_labels":{"machine-key":"Build Host"},"machine_aliases":{"local":"machine-key"}}`,
		)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	got, err := service.MachineLabels(
		t.Context(), servicehttp.NewHTTPBackend(server.URL, "", true, ""),
	)

	require.NoError(t, err)
	assert.Equal(t, "/api/v1/machines", gotPath)
	assert.Equal(t, service.MachineLabelCatalog{"machine-key": "Build Host"}, got)
}

func TestMachineLabelsUnsupportedServiceReturnsNil(t *testing.T) {
	var svc unsupportedMachineLabelsService

	got, err := service.MachineLabels(t.Context(), svc)

	require.NoError(t, err)
	assert.Nil(t, got)
}

type unsupportedMachineLabelsService struct {
	service.SessionService
}
