package parser

// Gemini Apps timestamp tests load IANA zones; trimmed test binaries and
// Windows have no system zone database.
import _ "time/tzdata"
