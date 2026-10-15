package codexcloudtest

import (
	"encoding/json/jsontext"
	"fmt"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostRecordsMessagesAfterAFailedReply(t *testing.T) {
	host := NewHost(t)
	release := make(chan struct{})
	host.Handle("block", func(jsontext.Value) (any, *Error) {
		<-release
		return nil, nil
	})
	//nolint:bodyclose // websocket.Dial owns the response body and sets it to nil on success
	c, _, err := websocket.Dial(t.Context(), host.URL().String(), nil)
	require.NoError(t, err)
	send := func(id int, method string) {
		t.Helper()
		msg := fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%d,"method":%q}`, id, method)
		require.NoError(t, c.Write(t.Context(), websocket.MessageText, msg))
	}

	send(1, "block")
	require.Eventually(t, func() bool { return len(host.Requests()) == 1 }, time.Second, 5*time.Millisecond)
	// The host replies to each of these after the client has gone, so its
	// writes start failing before it reads the last message.
	for id := 2; id <= 20; id++ {
		send(id, "unknown")
	}
	send(21, "last")
	require.NoError(t, c.CloseNow())
	close(release)

	require.Eventually(t, func() bool { return host.OpenConnections() == 0 }, 5*time.Second, 5*time.Millisecond)
	methods := host.Methods()
	require.Len(t, methods, 21)
	assert.Equal(t, "last", methods[20])
}
