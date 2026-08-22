//go:build !windows

package capture

func resolveChildCommand(command string) (string, error) {
	return command, nil
}
