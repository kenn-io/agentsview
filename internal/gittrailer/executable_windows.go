package gittrailer

// executable reports true because Git for Windows runs hooks without an execute bit.
func executable(string) bool { return true }
