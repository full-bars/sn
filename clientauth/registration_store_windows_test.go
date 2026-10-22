//go:build windows

package clientauth

// Real private directories and files under a per-test temporary directory
// prove the Windows custody contract against the Unix store's: the
// descriptor-relative component walk and its reparse refusal, the owner lock,
// the leaf and directory audits, and the durable publish and remove. The
// tokens and credentials here are synthetic.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// newRegistrationStorePath names a credential under a custody directory the
// store has to create for itself, so every test also exercises the walk.
func newRegistrationStorePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".urnetwork", "client.jwt")
}

func openTestStore(t *testing.T, path string) *registrationStore {
	t.Helper()
	store, err := openRegistrationStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}

// The directory this store creates is private on its own account: it is owned
// by the process account and its DACL is protected, so nothing it inherits
// from where it lives can widen it. An administrator's process owns what it
// creates through the token's default owner, which the store accepts.
func TestRegistrationStoreCreatesPrivateDirectory(t *testing.T) {
	path := newRegistrationStorePath(t)
	store := openTestStore(t, path)
	handle := windows.Handle(store.directory.Fd())
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	self, err := custodySelfSid()
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	if !custodyOwnerAcceptable(owner, self) {
		t.Fatal("custody directory is not owned by the process account")
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("custody directory DACL is not protected from inheritance")
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil || dacl.AceCount != 3 {
		t.Fatalf("custody directory DACL does not hold exactly its allowlist: %v", dacl)
	}
}

func TestRegistrationStoreRoundTrip(t *testing.T) {
	path := newRegistrationStorePath(t)
	store := openTestStore(t, path)
	if err := store.write("client.jwt", []byte("first")); err != nil {
		t.Fatal(err)
	}
	raw, err := store.read("client.jwt")
	if err != nil || string(raw) != "first" {
		t.Fatalf("read after write: %q %v", raw, err)
	}
	names, err := store.names(16)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names, "client.jwt") {
		t.Fatalf("census lost the published leaf: %v", names)
	}
	// publication replaces the complete bytes of an existing leaf
	if err := store.write("client.jwt", []byte("second")); err != nil {
		t.Fatal(err)
	}
	if raw, err := store.read("client.jwt"); err != nil || string(raw) != "second" {
		t.Fatalf("read after replace: %q %v", raw, err)
	}
	if err := store.remove("client.jwt"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read("client.jwt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed leaf still reads: %v", err)
	}
	// a missing leaf is already absent
	if err := store.remove("client.jwt"); err != nil {
		t.Fatalf("missing leaf removal: %v", err)
	}
	for _, name := range names {
		if strings.HasPrefix(name, ".registration-") {
			t.Fatalf("temporary leaf survived publication: %s", name)
		}
	}
}

// A held owner lock is the one thing a second store meets: its own open shares
// read, write and delete, so contention surfaces as the typed busy owner rather
// than as a raw sharing violation.
func TestRegistrationStoreOwnerLockContention(t *testing.T) {
	path := newRegistrationStorePath(t)
	owner := openTestStore(t, path)
	if _, err := openRegistrationStore(path); !errors.Is(err, errRegistrationOwnerActive) {
		t.Fatalf("second owner acquired custody: %v", err)
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	next, err := openRegistrationStore(path)
	if err != nil {
		t.Fatalf("custody stayed held after its owner closed: %v", err)
	}
	if err := next.close(); err != nil {
		t.Fatal(err)
	}
}

// The provider slot borrows the owner's directory rather than selecting a
// pathname again, and takes its own lock file so the owner keeps its own.
func TestRegistrationStoreProviderSlot(t *testing.T) {
	clientPath := newRegistrationStorePath(t)
	providerPath := filepath.Join(filepath.Dir(clientPath), "provider.jwt")
	owner := openTestStore(t, clientPath)
	child, err := openRegistrationStoreForOwner(providerPath, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.write("provider.jwt", []byte("provider")); err != nil {
		t.Fatal(err)
	}
	if raw, err := owner.read("provider.jwt"); err != nil || string(raw) != "provider" {
		t.Fatalf("owner cannot read the slot it lent: %q %v", raw, err)
	}
	if err := child.close(); err != nil {
		t.Fatal(err)
	}
	second, err := openRegistrationStoreForOwner(providerPath, owner)
	if err != nil {
		t.Fatalf("released slot stayed held: %v", err)
	}
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
}

// Every component is opened through its parent's handle, so a junction or
// symlink anywhere on the path is refused instead of followed.
func TestRegistrationStoreRefusesReparseComponent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		// a junction needs no privilege where a symlink does
		if out, junctionErr := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); junctionErr != nil {
			t.Skipf("no reparse point available: %v %s", err, out)
		}
	}
	path := filepath.Join(link, "custody", "client.jwt")
	if _, err := openRegistrationStore(path); err == nil || !strings.Contains(err.Error(), "reparse point") {
		t.Fatalf("walk entered a reparse component: %v", err)
	}
}

// A descriptor with no DACL grants every account full access, so custody
// refuses it exactly as it refuses a world-writable Unix directory.
func TestRegistrationStoreRefusesNullDacl(t *testing.T) {
	path := newRegistrationStorePath(t)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	setTestDirectorySecurity(t, dir, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil)
	if _, err := openRegistrationStore(path); err == nil || !strings.Contains(err.Error(), "not writable by other users") {
		t.Fatalf("adopted a null DACL: %v", err)
	}
}

// One allow ACE for an account outside the custody allowlist is enough to
// refuse the whole directory.
func TestRegistrationStoreRefusesForeignDacl(t *testing.T) {
	path := newRegistrationStorePath(t)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	self, err := custodySelfSid()
	if err != nil {
		t.Fatal(err)
	}
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	acl := privateCustodyACL([]*windows.SID{self, world})
	setTestDirectorySecurity(t, dir, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, (*windows.ACL)(unsafe.Pointer(&acl[0])))
	if _, err := openRegistrationStore(path); err == nil || !strings.Contains(err.Error(), "not writable by other users") {
		t.Fatalf("adopted a DACL that grants a foreign account: %v", err)
	}
}

// A symlink leaf is a leaf: reading it is refused, and removing it deletes the
// link itself, as unix unlinkat removes the link rather than its target.
func TestRegistrationStoreRefusesAndRemovesSymlinkLeaf(t *testing.T) {
	path := newRegistrationStorePath(t)
	store := openTestStore(t, path)
	outside := filepath.Join(t.TempDir(), "outside.jwt")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(store.path, "leaf.jwt")
	if err := os.Symlink(outside, leaf); err != nil {
		t.Skipf("no symlink available: %v", err)
	}
	if _, err := store.read("leaf.jwt"); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("read followed a symlink leaf: %v", err)
	}
	if err := store.remove("leaf.jwt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(leaf); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink leaf survived its removal: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("removal reached the symlink target: %v", err)
	}
}

// A publication that cannot complete still leaves the directory as it was: the
// temporary leaf is removed rather than leaked.
func TestRegistrationStoreRemovesTemporaryLeafWhenPublishFails(t *testing.T) {
	path := newRegistrationStorePath(t)
	store := openTestStore(t, path)
	if err := os.Mkdir(filepath.Join(store.path, "blocked.jwt"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.write("blocked.jwt", []byte("x")); err == nil {
		t.Fatal("published over a directory")
	}
	names, err := store.names(64)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if strings.HasPrefix(name, ".registration-") {
			t.Fatalf("temporary leaf survived a failed publication: %s", name)
		}
	}
}

func setTestDirectorySecurity(t *testing.T, dir string, information windows.SECURITY_INFORMATION, acl *windows.ACL) {
	t.Helper()
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, information, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}
