//go:build windows

package capture

import (
	"os"
	"path/filepath"
	"runtime"
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

	require.ErrorContains(t, err, "permits replacement")
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

	require.ErrorContains(t, err, "permits replacement")
}

func TestWindowsParentSecurityRejectsUntrustedOwner(t *testing.T) {
	allowed, err := captureDirectorySIDs()
	require.NoError(t, err)
	descriptor, err := windows.SecurityDescriptorFromString(
		"O:WDD:P(A;;GA;;;" + allowed[0].String() + ")",
	)
	require.NoError(t, err)

	err = verifyWindowsParentSecurity(descriptor, allowed)

	require.ErrorContains(t, err, "untrusted owner")
}

func TestWindowsParentSecurityAllowsTrustedInstallerOwner(t *testing.T) {
	allowed, err := captureDirectorySIDs()
	require.NoError(t, err)
	descriptor, err := windows.SecurityDescriptorFromString(
		"O:" + trustedInstallerSIDString + "D:P(A;;GA;;;" + allowed[0].String() + ")",
	)
	require.NoError(t, err)

	err = verifyWindowsParentSecurity(descriptor, allowed)

	require.NoError(t, err)
}

func TestWindowsParentSecurityAllowsInheritOnlyWrites(t *testing.T) {
	allowed, err := captureDirectorySIDs()
	require.NoError(t, err)
	descriptor, err := windows.SecurityDescriptorFromString(
		"O:" + allowed[0].String() + "D:P" +
			"(A;;GA;;;" + allowed[0].String() + ")" +
			"(A;CIIO;GW;;;WD)",
	)
	require.NoError(t, err)

	err = verifyWindowsParentSecurity(descriptor, allowed)

	require.NoError(t, err)
}

func TestWindowsCaptureDirectoryAllowsSplitTrustedEntries(t *testing.T) {
	allowed, err := captureDirectorySIDs()
	require.NoError(t, err)
	sddl := "D:P"
	for _, sid := range allowed {
		sddl += "(A;OICI;GA;;;" + sid.String() + ")"
	}
	sddl += "(A;CI;GA;;;" + allowed[0].String() + ")"
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	require.NoError(t, err)

	err = verifyCaptureDirectorySecurity(descriptor, allowed)

	require.NoError(t, err)
}

func TestWindowsCaptureDirectoryAllowsMappedFullAccess(t *testing.T) {
	allowed, err := captureDirectorySIDs()
	require.NoError(t, err)
	sddl := "D:P"
	for _, sid := range allowed {
		sddl += "(A;OICI;FA;;;" + sid.String() + ")"
	}
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	require.NoError(t, err)

	err = verifyCaptureDirectorySecurity(descriptor, allowed)

	require.NoError(t, err)
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
	var pinner runtime.Pinner
	defer pinner.Unpin()
	entries := make([]windows.EXPLICIT_ACCESS, 0, len(allowed))
	for _, sid := range allowed {
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
	require.NoError(t, err)
	require.NoError(t, windows.SetNamedSecurityInfo(
		providerRoot,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	))

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

func TestWindowsCaptureDirectoryRequiresInheritableTrustedAccess(t *testing.T) {
	allowed, err := captureDirectorySIDs()
	require.NoError(t, err)
	sddl := "D:P"
	for _, sid := range allowed {
		sddl += "(A;;GA;;;" + sid.String() + ")"
	}
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	require.NoError(t, err)

	err = verifyCaptureDirectorySecurity(descriptor, allowed)

	require.ErrorContains(t, err, "omits inheritable full access")
}

func TestWindowsClaudeProviderRootAllowsInheritedTrustedAccess(t *testing.T) {
	allowed, err := captureDirectorySIDs()
	require.NoError(t, err)
	sddl := "D:AI"
	for _, sid := range allowed {
		sddl += "(A;OICIID;GA;;;" + sid.String() + ")"
	}
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	require.NoError(t, err)

	err = verifyClaudeProviderRootSecurity(descriptor, allowed)

	require.NoError(t, err)
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
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return err
	}
	var pinner runtime.Pinner
	defer pinner.Unpin()
	pinner.Pin(world)
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(world),
		},
	}}, nil)
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
