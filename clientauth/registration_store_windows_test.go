//go:build windows

package clientauth

// Real private directories and files under a per-test temporary directory
// prove the Windows custody contract against the Unix store's: the
// descriptor-relative component walk and its reparse refusal, the owner lock,
// the leaf and directory audits, and the durable publish and remove. The
// tokens and credentials here are synthetic.

import (
	"encoding/binary"
	"errors"
	"fmt"
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
	if _, err := openRegistrationStore(path); err == nil || !strings.Contains(err.Error(), "icacls") {
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
	if _, err := openRegistrationStore(path); err == nil || !strings.Contains(err.Error(), "icacls") {
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
	if err := trySetTestPathSecurity(dir, information, acl, nil); err != nil {
		t.Fatal(err)
	}
}

// trySetTestPathSecurity rewrites the descriptor of one file or directory, so a
// test can plant the inherited, permissive or foreign ACE that a real host
// carries.
func trySetTestPathSecurity(path string, information windows.SECURITY_INFORMATION, acl *windows.ACL, owner *windows.SID) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, information, owner, nil, acl, nil)
}

// custodyTestAce is one ACE a test plants: a SID, the rights it grants and its
// inheritance flags.
type custodyTestAce struct {
	sid   *windows.SID
	mask  uint32
	flags uint8
}

// custodyTestAcl builds an ACL of the given entries, so a test can write the
// exact descriptor a host's inherited or foreign access produces.
func custodyTestAcl(entries ...custodyTestAce) []byte {
	size := 8
	for _, entry := range entries {
		size += aclAlignment(len(custodySidBytes(entry.sid)) + 8)
	}
	acl := make([]byte, size)
	acl[0] = aclRevision
	binary.LittleEndian.PutUint16(acl[2:], uint16(size))
	binary.LittleEndian.PutUint16(acl[4:], uint16(len(entries)))
	at := 8
	for _, entry := range entries {
		sid := custodySidBytes(entry.sid)
		length := aclAlignment(len(sid) + 8)
		acl[at] = byte(windows.ACCESS_ALLOWED_ACE_TYPE)
		acl[at+1] = entry.flags
		binary.LittleEndian.PutUint16(acl[at+2:], uint16(length))
		binary.LittleEndian.PutUint32(acl[at+4:], entry.mask)
		copy(acl[at+8:], sid)
		at += length
	}
	return acl
}

func testWellKnownSid(t *testing.T, kind windows.WELL_KNOWN_SID_TYPE) *windows.SID {
	t.Helper()
	sid, err := windows.CreateWellKnownSid(kind)
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

// A junction is refused on the component the walk opens whatever else the host
// allows. The earlier test tries a symlink first and an administrator's runner
// always succeeds at that, so this is the junction case on its own.
func TestRegistrationStoreRefusesJunctionComponent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("no junction available: %v %s", err, out)
	}
	if _, err := openRegistrationStore(filepath.Join(link, "custody", "client.jwt")); err == nil || !strings.Contains(err.Error(), "reparse point") {
		t.Fatalf("the walk entered a junction component: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "custody")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the walk wrote through the junction it refused: %v", err)
	}
}

// The custody directory itself as a junction is refused, never adopted: an
// adopted reparse point would place the private files wherever it points.
func TestRegistrationStoreRefusesReparseCustodyDirectory(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	custody := filepath.Join(root, ".urnetwork")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", custody, target).CombinedOutput(); err != nil {
		t.Skipf("no junction available: %v %s", err, out)
	}
	if _, err := openRegistrationStore(filepath.Join(custody, "client.jwt")); err == nil || !strings.Contains(err.Error(), "reparse point") {
		t.Fatalf("a junction custody directory was adopted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "client.jwt.registration.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the store wrote through the junction it refused: %v", err)
	}
}

// A leaf the store created can still gain a foreign ACE later: the leaf audit
// refuses a read any account outside custody holds, the 0600 bound.
func TestRegistrationStoreRefusesLeafWithForeignRead(t *testing.T) {
	path := newRegistrationStorePath(t)
	store := openTestStore(t, path)
	if err := store.write("client.jwt", []byte("synthetic")); err != nil {
		t.Fatal(err)
	}
	self, err := custodySelfSid()
	if err != nil {
		t.Fatal(err)
	}
	acl := custodyTestAcl(
		custodyTestAce{sid: self, mask: fileAllAccess},
		custodyTestAce{sid: testWellKnownSid(t, windows.WinWorldSid), mask: windows.FILE_GENERIC_READ},
	)
	if err := trySetTestPathSecurity(filepath.Join(store.path, "client.jwt"),
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, (*windows.ACL)(unsafe.Pointer(&acl[0])), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read("client.jwt"); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("read a leaf a foreign account may read: %v", err)
	}
}

// The realistic adoption failure: .urnetwork already exists with an inheritable
// ACE for BUILTIN\Users from its parent, which a data drive, ProgramData or a
// domain profile grants. The refusal must name the account, the right and the
// command that repairs the directory.
func TestRegistrationStoreRefusesInheritedForeignAce(t *testing.T) {
	root := t.TempDir()
	self, err := custodySelfSid()
	if err != nil {
		t.Fatal(err)
	}
	// both ACEs are inheritable, so the child carries them as inherited: the
	// account keeps full control, and the foreign account holds write
	acl := custodyTestAcl(
		custodyTestAce{sid: self, mask: fileAllAccess, flags: windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE},
		custodyTestAce{
			sid:   testWellKnownSid(t, windows.WinBuiltinUsersSid),
			mask:  windows.FILE_GENERIC_WRITE,
			flags: windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE,
		},
	)
	if err := trySetTestPathSecurity(root, windows.DACL_SECURITY_INFORMATION, (*windows.ACL)(unsafe.Pointer(&acl[0])), nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".urnetwork")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	_, err = openRegistrationStore(filepath.Join(dir, "client.jwt"))
	if err == nil {
		t.Fatal("adopted a directory whose DACL grants a foreign account")
	}
	message := err.Error()
	if !strings.Contains(message, "S-1-5-32-545") || !strings.Contains(message, "write") || !strings.Contains(message, "icacls") {
		t.Fatalf("the refusal is not actionable: %v", err)
	}
}

// A leaf owned by an account outside custody is refused, whatever its DACL
// says. Changing an owner to another account needs SeRestorePrivilege, so an
// unelevated host skips rather than fails.
func TestRegistrationStoreRefusesLeafWithForeignOwner(t *testing.T) {
	path := newRegistrationStorePath(t)
	store := openTestStore(t, path)
	if err := store.write("client.jwt", []byte("synthetic")); err != nil {
		t.Fatal(err)
	}
	foreign := testWellKnownSid(t, windows.WinLocalSystemSid)
	if err := trySetTestPathSecurity(filepath.Join(store.path, "client.jwt"),
		windows.OWNER_SECURITY_INFORMATION, nil, foreign); err != nil {
		t.Skipf("changing an owner to LocalSystem needs an elevated token: %v", err)
	}
	if _, err := store.read("client.jwt"); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("read a leaf a foreign account owns: %v", err)
	}
}

// The owner lock excludes another process, not only another handle: a second
// process that opens the same custody answers errRegistrationOwnerActive.
func TestRegistrationStoreOwnerLockExcludesAnotherProcess(t *testing.T) {
	path := newRegistrationStorePath(t)
	owner := openTestStore(t, path)
	command := exec.Command(os.Args[0], "-test.run=^TestRegistrationStoreOwnerLockHelper$", "-test.timeout=2m")
	command.Env = append(os.Environ(), "URNETWORK_CUSTODY_LOCK_HELPER="+path)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("the second process did not meet the held owner lock: %v %s", err, out)
	}
	if !strings.Contains(string(out), "owner-lock-held") {
		t.Fatalf("the second process answered something else: %s", out)
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	command = exec.Command(os.Args[0], "-test.run=^TestRegistrationStoreOwnerLockHelper$", "-test.timeout=2m")
	command.Env = append(os.Environ(), "URNETWORK_CUSTODY_LOCK_HELPER="+path, "URNETWORK_CUSTODY_LOCK_HELPER_OPEN=1")
	if out, err := command.CombinedOutput(); err != nil || !strings.Contains(string(out), "owner-lock-free") {
		t.Fatalf("the released lock did not free the custody: %v %s", err, out)
	}
}

// TestRegistrationStoreOwnerLockHelper is the second process of the lock test.
// It runs only when that test re-executes the binary with a path to open.
func TestRegistrationStoreOwnerLockHelper(t *testing.T) {
	path := os.Getenv("URNETWORK_CUSTODY_LOCK_HELPER")
	if path == "" {
		t.Skip("helper process of TestRegistrationStoreOwnerLockExcludesAnotherProcess")
	}
	_, err := openRegistrationStore(path)
	if os.Getenv("URNETWORK_CUSTODY_LOCK_HELPER_OPEN") != "" {
		if err != nil {
			t.Fatalf("the released custody did not open: %v", err)
		}
		fmt.Println("owner-lock-free")
		return
	}
	if !errors.Is(err, errRegistrationOwnerActive) {
		t.Fatalf("the second process did not meet the held owner lock: %v", err)
	}
	fmt.Println("owner-lock-held")
}

// A removal unlinks the name at once, as unix unlinkat does. Another handle
// that shares delete cannot keep the name visible as delete-pending, where a
// legacy delete-on-close removal makes the next open answer access-denied
// instead of not-found and every ErrNotExist branch takes the error path.
func TestRegistrationStoreRemoveLeavesNoPendingName(t *testing.T) {
	path := newRegistrationStorePath(t)
	store := openTestStore(t, path)
	if err := store.write("client.jwt", []byte("synthetic")); err != nil {
		t.Fatal(err)
	}
	held, err := store.open("client.jwt", leafOpenRead)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := store.remove("client.jwt"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read("client.jwt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a removed name is still visible while another handle holds it: %v", err)
	}
	names, err := store.names(64)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(names, "client.jwt") {
		t.Fatalf("a removed name is still in the census: %v", names)
	}
}

// A renewal fingerprints the file exactly as every later reader does, so the
// renewed token stays in its own lineage. A fingerprint read before the close
// can describe a last-write time the file no longer carries, and the lineage
// then matches no reader: the marker stops blocking and a revoked client is
// unblocked again.
func TestRenewNetworkTokenFingerprintStaysInItsLineage(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".urnetwork", "jwt")
	if err := WriteNetworkToken(path, "first-sign-in"); err != nil {
		t.Fatal(err)
	}
	previous := "first-sign-in"
	for round := range 3 {
		renewed := fmt.Sprintf("renewed-sign-in-%d", round)
		if ok, err := RenewNetworkToken(path, previous, renewed); err != nil || !ok {
			t.Fatalf("renewal %d: ok=%t err=%v", round, ok, err)
		}
		token, fingerprint, err := readNetworkCredential(path)
		if err != nil {
			t.Fatal(err)
		}
		if token != renewed {
			t.Fatalf("renewal %d left %q", round, token)
		}
		raw, err := os.ReadFile(path + ".lineage")
		if err != nil {
			t.Fatal(err)
		}
		members, err := parseNetworkCredentialLineage(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(members, fingerprint) {
			t.Fatalf("renewal %d: the reader's fingerprint %.12s is in no lineage of %d members", round, fingerprint, len(members))
		}
		previous = renewed
	}
}

// The token's default owner is a copy of the SID, not a pointer into the query
// buffer's frame: the predicates answer the same after other work has reused
// that frame and the stack has moved.
func TestCustodyDefaultOwnerSurvivesOtherWork(t *testing.T) {
	if _, err := custodySelfSid(); err != nil {
		t.Fatal(err)
	}
	owner := custodyAccounts.defaultOwner
	if owner == nil {
		t.Skip("this host's token names no default owner")
	}
	if !owner.IsValid() {
		t.Fatalf("the default owner is not a valid SID: %q", owner.String())
	}
	want := owner.String()
	self, err := custodySelfSid()
	if err != nil {
		t.Fatal(err)
	}
	if !custodyOwnerAcceptable(owner, self) || custodySidOutside(owner, self) {
		t.Fatalf("the predicate disagrees about the default owner %s", want)
	}
	// a fresh query, a copy of the copy, and other allocations between the two
	// comparisons: anything the returned pointer aliased is overwritten here
	fresh := tokenDefaultOwner(windows.GetCurrentProcessToken())
	if fresh == nil || fresh.String() != want {
		t.Fatalf("a second query answered %v, want %s", fresh, want)
	}
	for range 64 {
		buffer := make([]byte, 4096)
		for index := range buffer {
			buffer[index] = 0x5a
		}
		_ = buffer
	}
	again := custodyAccounts.defaultOwner
	if again.String() != want {
		t.Fatalf("the default owner changed to %s after other work, want %s", again.String(), want)
	}
	if !custodyOwnerAcceptable(again, self) {
		t.Fatal("the default owner stopped being acceptable after other work")
	}
	if custodySidOutside(again, self) {
		t.Fatalf("the default owner %s is outside its own allowlist", want)
	}
}
