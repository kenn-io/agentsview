package rawderive

import "errors"

// ParserRuntimeVersion changes when execution improvements make previously
// rejected sources processable without changing provider parse semantics.
// Include it in the processing version when scheduling hosted reparses.
const ParserRuntimeVersion = 1

// MaxMaterializedBytes bounds one reconstructed source, including companions
// and replay parents. Custody admission has its own, independent file limit.
const MaxMaterializedBytes int64 = 1 << 30

const (
	parserOutputLimit       = 512 << 20
	parserAddressSpaceLimit = 4 << 30
	parserDataLimit         = 1 << 30
	parserGoMemoryLimit     = "768MiB"
	parserSizeLimitExitCode = 82
)

// ErrSizeLimit identifies a deterministic size rejection. Retrying the same
// generation with the same processing version cannot remove this limit.
var ErrSizeLimit = errors.New("raw processing size limit exceeded")
