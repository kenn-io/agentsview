package servicehttp

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errReader fails every read with err, simulating a broken connection.
type errReader struct{ err error }

func (e errReader) Read(_ []byte) (int, error) { return 0, e.err }

const summaryEvent = "event: summary\n" +
	"data: {\"scanned\":2,\"with_secrets\":1,\"total_findings\":3," +
	"\"definite_findings\":2,\"candidate_findings\":1}\n\n"

func TestParseScanStream_SummaryReturned(t *testing.T) {
	t.Parallel()
	sum, err := parseScanStream(scanTestStream(t, strings.NewReader(summaryEvent)), nil)
	require.NoError(t, err)
	require.NotNil(t, sum)
	assert.Equal(t, 2, sum.Scanned)
	assert.Equal(t, 1, sum.WithSecrets)
	assert.Equal(t, 3, sum.TotalFindings)
	assert.Equal(t, 2, sum.DefiniteFindings)
	assert.Equal(t, 1, sum.CandidateFindings)
}

func TestParseScanStream_NoSummaryIsError(t *testing.T) {
	t.Parallel()
	// Stream ends cleanly after a progress tick but before any summary.
	raw := "event: progress\ndata: {\"scanned\":1,\"total\":2}\n\n"
	_, err := parseScanStream(scanTestStream(t, strings.NewReader(raw)), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stream ended before summary")
}

func TestParseScanStream_ReadErrorBeforeSummary(t *testing.T) {
	t.Parallel()
	_, err := parseScanStream(scanTestStream(t, errReader{err: errors.New("boom")}), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading stream")
}

func TestParseScanStream_ErrorEventWins(t *testing.T) {
	t.Parallel()
	raw := "event: error\ndata: scan aborted\n\n"
	_, err := parseScanStream(scanTestStream(t, strings.NewReader(raw)), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scan aborted")
}

func TestParseScanStream_SummaryDespiteTrailingReadError(t *testing.T) {
	t.Parallel()
	// A complete summary arrives, then the connection drops. The result
	// is valid, so the trailing read error must not mask it.
	r := io.MultiReader(strings.NewReader(summaryEvent),
		errReader{err: errors.New("late boom")})
	sum, err := parseScanStream(scanTestStream(t, r), nil)
	require.NoError(t, err)
	require.NotNil(t, sum)
	assert.Equal(t, 2, sum.Scanned)
}

func TestParseScanStream_MalformedSummaryIsError(t *testing.T) {
	t.Parallel()
	raw := "event: summary\ndata: {not valid json}\n\n"
	_, err := parseScanStream(scanTestStream(t, strings.NewReader(raw)), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding summary")
}

func scanTestStream(t *testing.T, r io.Reader) *runtime.Stream[[]byte] {
	t.Helper()
	stream := runtime.NewEventStream[[]byte](&http.Response{Body: io.NopCloser(r)})
	t.Cleanup(func() { require.NoError(t, stream.Close()) })
	return stream
}
