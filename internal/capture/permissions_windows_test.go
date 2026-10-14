//go:build windows

package capture

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestCaptureDirectoryReplacesPermissiveDACLBeforeUse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "capture")
	state, err := createState(dir, windowsPermissionManifest(t, "windows-private"))
	require.NoError(t, err)
	state.close()
	assertProtectedCaptureDACL(t, dir)

	require.NoError(t, setPermissiveCaptureDACL(dir))
	recovered, err := openState(dir)
	require.NoError(t, err)
	recovered.close()
	assertProtectedCaptureDACL(t, dir)
}

func TestCreateStateRejectsReplaceableParentDACL(t *testing.T) {
	parent := t.TempDir()
	require.NoError(t, setPermissiveCaptureDACL(parent))

	_, err := createState(
		filepath.Join(parent, "capture"),
		windowsPermissionManifest(t, "windows-unsafe-parent"),
	)

	require.ErrorContains(t, err, "validating capture parent safety")
	assert.ErrorContains(t, err, "permits replacement")
}

func TestOpenStateRejectsParentDACLMadeReplaceable(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "capture")
	state, err := createState(
		dir, windowsPermissionManifest(t, "windows-parent-recovery"))
	require.NoError(t, err)
	state.close()
	require.NoError(t, setPermissiveCaptureDACL(parent))

	_, err = openState(dir)

	require.ErrorContains(t, err, "validating capture parent safety")
	assert.ErrorContains(t, err, "permits replacement")
}

func TestWindowsParentSecurity(t *testing.T) {
	allowed, err := captureDirectorySIDs()
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		sddl    string
		wantErr string
	}{
		{
			name:    "rejects untrusted owner",
			sddl:    "O:WDD:P(A;;GA;;;" + allowed[0].String() + ")",
			wantErr: "untrusted owner",
		},
		{
			name: "allows TrustedInstaller owner",
			sddl: "O:" + trustedInstallerSIDString + "D:P(A;;GA;;;" + allowed[0].String() + ")",
		},
		{
			name: "allows inherit-only full access",
			sddl: "O:" + allowed[0].String() + "D:P(A;;GA;;;" + allowed[0].String() + ")(A;CIIO;GA;;;WD)",
		},
		{
			name:    "rejects replacement by another principal",
			sddl:    "O:" + allowed[0].String() + "D:P(A;;GA;;;WD)",
			wantErr: "permits replacement",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			descriptor, err := windows.SecurityDescriptorFromString(tc.sddl)
			require.NoError(t, err)
			err = verifyWindowsParentSecurity(descriptor, allowed)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestWindowsCaptureDirectorySecurity(t *testing.T) {
	allowed, err := captureDirectorySIDs()
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		sddl    string
		wantErr string
	}{
		{
			name: "allows split trusted entries",
			sddl: allowedSDDL("D:P", "A;OICI;GA", allowed) + "(A;CI;GA;;;" + allowed[0].String() + ")",
		},
		{
			name: "allows mapped full access",
			sddl: allowedSDDL("D:P", "A;OICI;FA", allowed),
		},
		{
			name:    "requires inheritable trusted access",
			sddl:    allowedSDDL("D:P", "A;;GA", allowed),
			wantErr: "omits inheritable full access",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			descriptor, err := windows.SecurityDescriptorFromString(tc.sddl)
			require.NoError(t, err)
			err = verifyCaptureDirectorySecurity(descriptor, allowed)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestClaudeProviderRootCreatedWithProtectedInheritableDACL(t *testing.T) {
	providerRoot := filepath.Join(t.TempDir(), "claude")

	_, err := prepareClaudeReservationDirectory(providerRoot)

	require.NoError(t, err)
	assertProtectedCaptureDACL(t, providerRoot)
}

func TestClaudeProviderRootRejectsExistingPermissiveDACL(t *testing.T) {
	providerRoot := filepath.Join(t.TempDir(), "claude")
	require.NoError(t, setPermissiveCaptureDACL(providerRoot))

	_, err := prepareClaudeReservationDirectory(providerRoot)

	require.ErrorContains(t, err, "validating Claude provider root")
	assert.ErrorContains(t, err, "grants sensitive access")
}

func TestClaudeProviderRootAllowsExistingSafeUnprotectedDACL(t *testing.T) {
	providerRoot := t.TempDir()
	allowed, err := captureDirectorySIDs()
	require.NoError(t, err)
	require.NoError(t, setCaptureDACL(providerRoot, allowed))

	_, err = prepareClaudeReservationDirectory(providerRoot)

	require.NoError(t, err)
	descriptor, err := windows.GetNamedSecurityInfo(
		providerRoot, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	control, _, err := descriptor.Control()
	require.NoError(t, err)
	assert.Zero(t, control&windows.SE_DACL_PROTECTED,
		"existing provider ACLs must be validated without being rewritten")
}

func TestWindowsClaudeProviderRootSecurity(t *testing.T) {
	allowed, err := captureDirectorySIDs()
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		sddl    string
		wantErr string
	}{
		{
			name: "allows inherited trusted access",
			sddl: allowedSDDL("D:AI", "A;OICIID;GA", allowed),
		},
		{
			name:    "rejects sensitive access by another principal",
			sddl:    "D:P(A;;GA;;;WD)",
			wantErr: "grants sensitive access",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			descriptor, err := windows.SecurityDescriptorFromString(tc.sddl)
			require.NoError(t, err)
			err = verifyClaudeProviderRootSecurity(descriptor, allowed)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func windowsPermissionManifest(t *testing.T, occurrence string) manifest {
	t.Helper()
	return manifest{
		OccurrenceID:      occurrence,
		Provider:          string(ProviderClaude),
		ProviderSessionID: "11111111-1111-4111-8111-111111111111",
		ProviderRoot:      t.TempDir(),
		ProviderWorkDir:   t.TempDir(),
		StartedAt:         time.Now(),
		Invocation:        invocationName(ProviderClaude),
		Limits:            DefaultLimits(),
	}
}

func setPermissiveCaptureDACL(path string) error {
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return err
	}
	return setCaptureDACL(path, []*windows.SID{world})
}

func setCaptureDACL(path string, sids []*windows.SID) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	var pinner runtime.Pinner
	defer pinner.Unpin()
	entries := make([]windows.EXPLICIT_ACCESS, 0, len(sids))
	for _, sid := range sids {
		pinner.Pin(sid)
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	)
}

func assertProtectedCaptureDACL(t *testing.T, path string) {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(
		path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	control, _, err := descriptor.Control()
	require.NoError(t, err)
	assert.NotZero(t, control&windows.SE_DACL_PROTECTED)
	sddl := descriptor.String()
	assert.NotContains(t, sddl, ";;;WD)")
	assert.Contains(t, sddl, ";;;SY)")
	assert.Contains(t, sddl, ";;;BA)")
	allowed, err := captureDirectorySIDs()
	require.NoError(t, err)
	require.NoError(t, verifyCaptureDirectoryDACL(path, allowed))
}

// allowedSDDL builds a DACL string that grants ace to every allowed SID.
func allowedSDDL(header, ace string, allowed []*windows.SID) string {
	var sddl strings.Builder
	sddl.WriteString(header)
	for _, sid := range allowed {
		sddl.WriteString("(" + ace + ";;;" + sid.String() + ")")
	}
	return sddl.String()
}
