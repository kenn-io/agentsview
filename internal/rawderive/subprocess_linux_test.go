//go:build linux && (amd64 || arm64)

package rawderive

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if handled, code := RunParserChild(os.Args[1:]); handled {
		os.Exit(code)
	}
	if len(os.Args) > 1 && os.Args[1] == "--sandbox-fd-test" {
		if os.Getenv("AGENTSVIEW_RAW_PARSER_CHILD") == "1" && !parserFDBootstrapCompleted() {
			os.Exit(81)
		}
		fd, _ := strconv.Atoi(os.Args[2])
		data := make([]byte, 64)
		n, err := unix.Read(fd, data)
		if err == nil && string(data[:n]) == "outside descriptor sentinel" {
			os.Exit(80)
		}
		if len(os.Args) > 3 {
			sock, _ := strconv.Atoi(os.Args[3])
			if n, e := unix.Write(sock, []byte("outside socket sentinel")); e == nil && n > 0 {
				os.Exit(80)
			}
		}
		os.Stdout.WriteString("closed")
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "--sandbox-kernel-test" {
		os.Exit(sandboxKernelHelper(os.Args[2:]))
	}
	os.Exit(m.Run())
}
func sandboxKernelHelper(args []string) int {
	mode, source, jail, outside := args[0], args[1], args[2], args[3]
	// Keep several existing threads live across filter installation.
	gate := make(chan struct{})
	results := make(chan bool, 4)
	ready := make(chan struct{}, 4)
	for range 4 {
		go func() {
			runtime.LockOSThread()
			ready <- struct{}{}
			<-gate
			fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
			if fd >= 0 {
				unix.Close(fd)
			}
			results <- err == unix.EPERM
		}()
	}
	for range 4 {
		<-ready
	}
	var isolationErr error
	if mode == "seccomp" {
		isolationErr = installParserSeccomp()
	} else {
		isolationErr = isolateParser(source, jail)
	}
	if isolationErr != nil {
		return 78
	}
	os.Stdout.WriteString("READY\n")
	switch mode {
	case "cpu":
		//nolint:staticcheck // SA5002: deliberate CPU burn verifies the child's hard CPU limit.
		for {
		}
	case "memory":
		var held [][]byte
		for {
			b := make([]byte, 8<<20)
			for i := range b {
				b[i] = 1
			}
			held = append(held, b)
			runtime.KeepAlive(held)
		}
	case "denials", "seccomp":
		checks := map[string]bool{}
		if mode == "denials" {
			body, err := os.ReadFile("/source/input")
			checks["source_read"] = err == nil && string(body) == "fixture"
			_, err = os.ReadFile(outside)
			checks["outside_read"] = err != nil
			_ = os.Chmod("/source/input", 0600)
			checks["source_write"] = os.WriteFile("/source/input", []byte("bad"), 0600) != nil
			checks["jail_write"] = os.WriteFile("/escape", []byte("bad"), 0600) != nil
			_, err = unix.Mmap(-1, 0, 3<<30, unix.PROT_NONE, unix.MAP_PRIVATE|unix.MAP_ANON)
			checks["address_limit"] = err == unix.ENOMEM
			_, err = unix.Mmap(-1, 0, 600<<20, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
			checks["data_limit"] = err == unix.ENOMEM
		}

		for _, domain := range []int{unix.AF_INET, unix.AF_INET6, unix.AF_UNIX} {
			fd, e := unix.Socket(domain, unix.SOCK_DGRAM, 0)
			if fd >= 0 {
				unix.Close(fd)
			}
			checks["socket_"+string(rune('a'+domain))] = e == unix.EPERM
		}
		close(gate)
		for range 4 {
			checks["threads"] = <-results
			if !checks["threads"] {
				return 80
			}
		}
		fresh := make(chan bool)
		go func() {
			runtime.LockOSThread()
			fd, e := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			if fd >= 0 {
				unix.Close(fd)
			}
			fresh <- e == unix.EPERM
		}()
		checks["new_thread"] = <-fresh
		checks["exec"] = unix.Exec("/source/input", []string{"input"}, nil) == unix.EPERM
		pid, _, errno := unix.RawSyscall(unix.SYS_CLONE, uintptr(unix.SIGCHLD), 0, 0)
		if errno == 0 && pid == 0 {
			os.Exit(90)
		}
		checks["fork"] = errno == unix.EPERM
		checks["unshare"] = unix.Unshare(unix.CLONE_NEWNET) == unix.EPERM
		checks["mount"] = unix.Mount("", "/", "", unix.MS_REMOUNT, "") == unix.EPERM
		checks["raise_limit"] = unix.Setrlimit(unix.RLIMIT_AS, &unix.Rlimit{Cur: 3 << 30, Max: 3 << 30}) == unix.EPERM
		checks["environment"] = os.Getenv("RAW_SANDBOX_SECRET") == ""
		checks["foreign_signal"] = unix.Kill(1, 0) == unix.EPERM
		json.NewEncoder(os.Stdout).Encode(checks)
		return 0
	}
	return 80
}
func TestSandboxKernelControls(t *testing.T) {
	p, err := NewSubprocessParser(5 * time.Second)
	require.NoError(t, err)
	if err = p.Preflight(t.Context()); err != nil {
		require.ErrorIs(t, err, ErrSandboxUnavailable)
		if os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
			t.Fatal("mandatory kernel isolation unavailable")
		}
		t.Skip("kernel isolation unavailable; positive controls require Linux namespace support")
	}
	t.Setenv("RAW_SANDBOX_SECRET", "must-not-inherit")
	for _, mode := range []string{"denials", "memory", "cpu"} {
		t.Run(mode, func(t *testing.T) {
			source := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(source, "input"), []byte("fixture"), 0400))
			jail := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(jail, "source"), 0700))
			outside := filepath.Join(t.TempDir(), "sentinel")
			require.NoError(t, os.WriteFile(outside, []byte("outside"), 0400))
			ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "--sandbox-kernel-test", mode, source, jail, outside)
			cmd.Env = []string{"GOMAXPROCS=2", "GOMEMLIMIT=384MiB"}
			require.NoError(t, configureParserNamespace(cmd))
			start := time.Now()
			out, err := runParserProcess(ctx, cancel, cmd, nil, parserOutputLimit, parserErrorLimit)
			require.NotNil(t, cmd.ProcessState)
			if mode == "denials" {
				require.NoError(t, err)
				var checks map[string]bool
				require.NoError(t, json.Unmarshal(out, &checks))
				require.GreaterOrEqual(t, len(checks), 14)
				for key, value := range checks {
					assert.True(t, value, key)
				}
			} else {
				require.Error(t, err)
				assert.NoError(t, ctx.Err())
				assert.Less(t, time.Since(start), 38*time.Second)
				if mode == "cpu" {
					status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
					require.True(t, ok)
					assert.Equal(t, syscall.SIGKILL, status.Signal())
					assert.Greater(t, time.Since(start), 20*time.Second)
				} else {
					assert.Equal(t, 2, cmd.ProcessState.ExitCode(), "Go hard allocation failure")
				}
			}
			body, err := os.ReadFile(filepath.Join(source, "input"))
			require.NoError(t, err)
			assert.Equal(t, "fixture", string(body))
			require.NoError(t, p.Preflight(t.Context()), "subsequent jobs remain usable")
		})
	}
}
func TestSandboxActivationRefusesMissingIsolation(t *testing.T) {
	p, err := NewSubprocessParser(time.Second)
	require.NoError(t, err)
	cmd := exec.Command("true")
	if !parserFDBootstrapAvailable {
		require.ErrorIs(t, configureParserNamespace(cmd), ErrSandboxUnavailable)
		require.ErrorIs(t, p.Preflight(t.Context()), ErrSandboxUnavailable)
		return
	}
	require.NoError(t, configureParserNamespace(cmd))
	if err = cmd.Run(); err == nil {
		t.Skip("host supports namespaces")
	}
	require.ErrorIs(t, p.Preflight(t.Context()), ErrSandboxUnavailable)
}

func TestSandboxProviderJSONAndSQLite(t *testing.T) {
	p, err := NewSubprocessParser(20 * time.Second)
	require.NoError(t, err)
	if err = p.Preflight(t.Context()); err != nil {
		if os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
			t.Fatal(err)
		}
		t.Skip("kernel isolation unavailable")
	}
	fixtures := []struct {
		name      string
		provider  parser.AgentType
		path, key string
		body      []byte
		ids       []string
	}{
		{"json", parser.AgentClaude, "project/session-a.jsonl", "/canonical/project/session-a.jsonl", []byte(`{"type":"user","timestamp":"2026-08-13T12:00:00Z","uuid":"u1","sessionId":"session-a","message":{"content":"hello"},"cwd":"/work/project"}` + "\n" + `{"type":"assistant","timestamp":"2026-08-13T12:00:01Z","uuid":"a1","parentUuid":"u1","sessionId":"session-a","message":{"content":"hi"}}` + "\n"), []string{"session-a"}},
		{"sqlite", parser.AgentForge, parser.ForgeDBFilename, parser.ForgeDBFilename, forgeSnapshotFixture(t, map[string]string{"conv-001": "2026-05-02 09:58:15", "conv-002": "2026-05-03 09:58:15"}), []string{"forge:conv-001", "forge:conv-002"}},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			ref := objectRefForBytes(t, f.body)
			m := parserTestManifest(t, f.provider, f.key, []rawsync.Entry{{Path: f.path, Type: "file", Length: int64(len(f.body)), Objects: []rawsync.ObjectRef{ref}}})
			tree, err := (Materializer{Store: &materializerStore{objects: map[rawsync.ObjectRef][]byte{ref: f.body}}, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20}).Materialize(t.Context(), m)
			require.NoError(t, err)
			defer func() { require.NoError(t, tree.Cleanup()) }()
			got, err := p.Parse(t.Context(), m, tree)
			require.NoError(t, err)
			require.Len(t, got.Outcome.Results, len(f.ids))
			assert.True(t, got.Outcome.ResultSetComplete)
			assert.Empty(t, got.Outcome.SourceErrors)
			for i, id := range f.ids {
				assert.Equal(t, id, got.Outcome.Results[i].Result.Session.ID)
				require.Len(t, got.Outcome.Results[i].Result.Messages, 2)
			}
			plain, err := NewProviderParser(parser.ProviderFactories(), "hosted")
			require.NoError(t, err)
			expected, err := plain.Parse(t.Context(), m, tree)
			require.NoError(t, err)
			assert.Equal(t, expected, got, "kernel boundary preserves complete normalized provider semantics")
		})
	}
}

func TestSandboxClosesDeliberatelyInheritedDescriptor(t *testing.T) {
	if !parserFDBootstrapAvailable {
		t.Skip("cgo pre-runtime FD bootstrap unavailable")
	}
	path := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(path, []byte("outside descriptor sentinel"), 0400))
	// unix.Open without O_CLOEXEC is deliberate: os.Open would hide the bug.
	fd, err := unix.Open(path, unix.O_RDONLY, 0)
	require.NoError(t, err)
	defer unix.Close(fd)
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	require.NoError(t, err)
	defer unix.Close(pair[0])
	defer unix.Close(pair[1])
	require.NoError(t, unix.SetNonblock(pair[1], true))
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	require.NoError(t, err)
	require.Zero(t, flags&unix.FD_CLOEXEC)
	control := exec.Command(os.Args[0], "--sandbox-fd-test", strconv.Itoa(fd))
	control.Env = []string{"GOMAXPROCS=2"}
	err = control.Run()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 80, exit.ExitCode(), "control must actually inherit the sentinel")
	cmd := exec.Command(os.Args[0], "--sandbox-fd-test", strconv.Itoa(fd), strconv.Itoa(pair[0]))
	cmd.Env = []string{"GOMAXPROCS=2", "AGENTSVIEW_RAW_PARSER_CHILD=1"}
	socketControl := exec.Command(os.Args[0], "--sandbox-fd-test", "-1", strconv.Itoa(pair[0]))
	socketControl.Env = []string{"GOMAXPROCS=2"}
	err = socketControl.Run()
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 80, exit.ExitCode(), "control must inherit the socket")
	received := make([]byte, 64)
	n, err := unix.Read(pair[1], received)
	require.NoError(t, err)
	assert.Equal(t, "outside socket sentinel", string(received[:n]))
	_, err = unix.Seek(fd, 0, 0)
	require.NoError(t, err)
	output, err := cmd.Output()
	require.NoError(t, err)
	assert.Equal(t, "closed", string(output))
	data := make([]byte, 64)
	n, err = unix.Read(fd, data)
	require.NoError(t, err)
	assert.Equal(t, "outside descriptor sentinel", string(data[:n]), "parent descriptor remains usable")
	_, err = unix.Read(pair[1], data)
	assert.ErrorIs(t, err, unix.EAGAIN, "child cannot use inherited socket")
	require.False(t, parserFDBootstrapCompleted(), "ordinary parent must retain its descriptors")
}

func TestSandboxSeccompSynchronizesThreads(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "--sandbox-kernel-test", "seccomp", "unused", "unused", "unused")
	cmd.Env = []string{"GOMAXPROCS=2", "AGENTSVIEW_RAW_PARSER_CHILD=1"}
	out, err := runParserProcess(ctx, cancel, cmd, nil, 4096, parserErrorLimit)
	require.NoError(t, err)
	var checks map[string]bool
	require.NoError(t, json.Unmarshal(out, &checks))
	require.GreaterOrEqual(t, len(checks), 10)
	for name, value := range checks {
		assert.True(t, value, name)
	}
}
