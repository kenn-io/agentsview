package main

// Existing command outcomes stay distinct from the new CLI error classes.
// These values are part of the scripting contract in the command reference.
const (
	tokenUseExitOK                      = 0
	tokenUseExitErr                     = 1
	tokenUseExitNotFound                = 2
	tokenUseExitNoTokenData             = 3
	dataVersionTooNewExitCode           = 3
	rawSyncBackfillExitCode             = 2
	sessionExportCursorResetExitCode    = 4
	conversationRevisionChangedExitCode = 5

	cliUsageExitCode       = 64
	cliValidationExitCode  = 65
	cliUnavailableExitCode = 69
)
