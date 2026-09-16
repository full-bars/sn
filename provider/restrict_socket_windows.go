//go:build windows

package provider

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// restrictSocketACL sets a Windows DACL on the control socket file so
// that only the current user can connect. On Windows, os.Chmod 0600 is
// a no-op — without an explicit DACL every local user can reach the
// socket, which means any user on the machine could change provider
// settings.
func restrictSocketACL(path string) error {
	return restrictFileACL(path)
}

// restrictFileACL grants the current user GENERIC_ALL on path via a
// DACL. Everyone else is implicitly denied because the DACL contains
// only one ACE — the owner's. PROTECTED_DACL_SECURITY_INFORMATION is
// set to block inherited ACEs from parent directories (otherwise users
// like BUILTIN\Users or Authenticated Users gain implicit access via
// NTFS inheritance). GENERIC_ALL includes DELETE so os.Remove(path)
// works during socket cleanup even if the parent directory does not
// grant FILE_DELETE_CHILD.
func restrictFileACL(path string) error {
	// Current user SID from the process token.
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return fmt.Errorf("open process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("get token user: %w", err)
	}
	sid := user.User.Sid

	// ACEs: grant the current user full access, plus allow SYSTEM and
	// Administrators so that elevated services and admin tools can still
	// manage the socket (e.g. Windows services, scheduled tasks).
	systemSID, _ := windows.StringToSid("S-1-5-18")     // NT AUTHORITY\SYSTEM
	adminsSID, _ := windows.StringToSid("S-1-5-32-544") // BUILTIN\Administrators

	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(systemSID),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(adminsSID),
			},
		},
	}

	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("build DACL: %w", err)
	}

	// Apply the DACL to the file. PROTECTED_DACL_SECURITY_INFORMATION
	// prevents inherited ACEs from parent directories from merging into
	// the new DACL. Without this flag, any permissions granted to
	// BUILTIN\Users, Authenticated Users, or Everyone via the containing
	// directory would remain active on provider.sock — enabling any
	// local user to connect and issue commands (including shutdown).
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	); err != nil {
		return fmt.Errorf("set named security info: %w", err)
	}

	return nil
}
