package parser

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkDiscoveryDiskMap(b *testing.B) {
	for _, operation := range []string{"get", "put", "append"} {
		b.Run(operation, func(b *testing.B) {
			index, err := newDiscoveryDiskMap(b.Context())
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, index.close()) })
			value := strings.Repeat("Synthetic metadata. ", 8)
			_, err = index.db.ExecContext(b.Context(), `
				WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 10000)
				INSERT INTO entries (key, ordinal, value)
				SELECT printf('key-%05d', i), 0, ? FROM n`, value)
			require.NoError(b, err)
			keys := make([]string, 10000)
			for i := range keys {
				keys[i] = fmt.Sprintf("key-%05d", i+1)
			}
			_, _, err = index.get(b.Context(), keys[0])
			require.NoError(b, err)
			require.NoError(b, index.put(b.Context(), keys[0], value, true))
			require.NoError(b, index.append(b.Context(), "warm-append", "span"))
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				key := keys[i%len(keys)]
				switch operation {
				case "get":
					got, found, err := index.get(b.Context(), key)
					require.NoError(b, err)
					require.True(b, found)
					require.Equal(b, value, got)
				case "put":
					require.NoError(b, index.put(b.Context(), key, value, true))
				case "append":
					require.NoError(b, index.append(b.Context(), key, "span"))
				}
			}
		})
	}
}
