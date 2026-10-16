// Package remotesource picks the transport for a configured remote host.
package remotesource

import (
	"errors"
	"fmt"
	"os"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/remotesync"
	"go.kenn.io/agentsview/internal/sshremote"
)

// Options describe the local collector.
type Options struct {
	// Version is the collector's build version.
	Version string
	// DataDir holds the downloaded release cache for SSH hosts.
	DataDir string
}

// For returns a new Source for one sync of rh. Callers name the host when
// they report an error, so the messages here leave it out.
func For(rh config.RemoteHost, opts Options) (remotesync.Source, error) {
	switch rh.Transport {
	case "", config.RemoteTransportHTTP:
		if rh.Token == "" {
			return nil, errors.New("http remote sync token is required")
		}
		return remotesync.HTTPSource{URL: rh.URL, Token: rh.Token}, nil
	case config.RemoteTransportSSH:
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate agentsview executable: %w", err)
		}
		// The source reports its upload through RemoteSync's progress
		// callback, which RemoteSync hands it (remotesync.ProgressReporter).
		return sshremote.NewSource(sshremote.Provisioner{
			Runner:  sshremote.Runner{Destination: rh.SSHDestination()},
			Build:   sshremote.LocalBuild{Version: opts.Version, Executable: exe},
			DataDir: opts.DataDir,
			Host:    rh.Host,
		}), nil
	default:
		return nil, fmt.Errorf("invalid remote transport %q; use http or ssh", rh.Transport)
	}
}
