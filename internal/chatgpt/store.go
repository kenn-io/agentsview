package chatgpt

import (
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/atomicfile"
)

const credentialLimit = 256 << 10

// Status contains no account identifiers or secrets. Expiry can be in the past:
// the next generation request refreshes an otherwise connected credential.
type Status struct {
	State     string    `json:"state"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

type credential struct {
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	AccountID    string    `json:"account_id,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
	Reconnect    bool      `json:"reconnect_required,omitempty"`
}

func (c *Client) directory() string { return filepath.Join(c.dataDir, "insights-chatgpt") }

func privateMode(info os.FileInfo) bool {
	return runtime.GOOS == "windows" || info.Mode().Perm()&0o077 == 0
}

func (c *Client) secureDirectory(ctx context.Context, create bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.dataDir == "" {
		return errors.New("ChatGPT data directory is required")
	}
	if create {
		if err := os.MkdirAll(c.directory(), 0o700); err != nil {
			return errors.New("cannot create private ChatGPT credential directory")
		}
	}
	info, err := os.Lstat(c.directory())
	if errors.Is(err, os.ErrNotExist) {
		return os.ErrNotExist
	}
	if err != nil || !info.IsDir() || !privateMode(info) {
		return errors.New("ChatGPT credential directory must be private (0700) and not a symlink")
	}
	return nil
}

func (c *Client) readPrivate(ctx context.Context, name string) ([]byte, error) {
	if err := c.secureDirectory(ctx, false); err != nil {
		return nil, err
	}
	path := filepath.Join(c.directory(), name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, os.ErrNotExist
	}
	if err != nil || !info.Mode().IsRegular() || !privateMode(info) {
		return nil, errors.New("ChatGPT credential files must be private (0600) regular files")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read ChatGPT credential file")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, credentialLimit+1))
	if err != nil || len(data) > credentialLimit {
		return nil, errors.New("invalid ChatGPT credential file")
	}
	return data, nil
}

func (c *Client) writePrivate(ctx context.Context, name string, data []byte) error {
	if len(data) > credentialLimit {
		return errors.New("ChatGPT credential file too large")
	}
	if err := c.secureDirectory(ctx, true); err != nil {
		return err
	}
	f, err := os.CreateTemp(c.directory(), ".pending-")
	if err != nil {
		return errors.New("cannot create ChatGPT credential file")
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("cannot write ChatGPT credential file")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err = atomicfile.Replace(f.Name(), filepath.Join(c.directory(), name)); err != nil {
		return errors.New("cannot replace ChatGPT credential file")
	}
	return nil
}

func (c *Client) withLock(ctx context.Context, fn func() error) error {
	if err := c.secureDirectory(ctx, true); err != nil {
		return err
	}
	// This inode stays in place through credential replacement and logout.
	path := filepath.Join(c.directory(), ".lock")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || !privateMode(info) {
			return errors.New("ChatGPT lock must be a private regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect ChatGPT credential lock")
	}
	lock := flock.New(path, flock.SetPermissions(0o600))
	ok, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil || !ok {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("cannot lock ChatGPT credentials")
	}
	defer func() { _ = lock.Unlock() }() // The OS releases it on process exit too.
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func (c *Client) loadCredential(ctx context.Context) (credential, error) {
	data, err := c.readPrivate(ctx, "credentials.json")
	if errors.Is(err, os.ErrNotExist) {
		return credential{}, errors.New("ChatGPT is not connected; run agentsview insight chatgpt login")
	}
	if err != nil {
		return credential{}, err
	}
	var cr credential
	if json.Unmarshal(data, &cr) != nil || (!cr.Reconnect && (cr.AccessToken == "" || cr.RefreshToken == "" || cr.AccountID == "" || cr.ExpiresAt.IsZero())) {
		return credential{}, errors.New("ChatGPT credentials are invalid; run agentsview insight chatgpt login")
	}
	return cr, nil
}

func (c *Client) saveCredential(ctx context.Context, cr credential) error {
	data, err := json.Marshal(cr)
	if err != nil {
		return errors.New("cannot encode ChatGPT credentials")
	}
	return c.writePrivate(ctx, "credentials.json", data)
}

// Status reads local state without refreshing or contacting OpenAI.
func (c *Client) Status(ctx context.Context) (Status, error) {
	// Avoid creating the directory or lock for a disconnected installation.
	if err := c.secureDirectory(ctx, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Status{State: "disconnected"}, nil
		}
		return Status{}, err
	}
	if _, err := os.Lstat(filepath.Join(c.directory(), "credentials.json")); errors.Is(err, os.ErrNotExist) {
		return Status{State: "disconnected"}, nil
	} else if err != nil {
		return Status{}, errors.New("cannot inspect ChatGPT credential file")
	}
	var status Status
	err := c.withLock(ctx, func() error {
		cr, err := c.loadCredential(ctx)
		if err != nil {
			return err
		}
		status = Status{State: "connected", ExpiresAt: cr.ExpiresAt}
		if cr.Reconnect {
			status = Status{State: "reconnect_required"}
		}
		return nil
	})
	return status, err
}

// Logout clears local credentials and cancels any pending login attempt. It
// cannot revoke already-issued tokens at OpenAI or stop in-flight inference.
func (c *Client) Logout(ctx context.Context) error {
	return c.withLock(ctx, func() error {
		if err := c.writePrivate(ctx, "revision", []byte(rand.Text())); err != nil {
			return err
		}
		err := os.Remove(filepath.Join(c.directory(), "credentials.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("cannot remove ChatGPT credentials")
		}
		return nil
	})
}
