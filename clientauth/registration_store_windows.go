//go:build windows

package clientauth

// One owner-private directory handle and nonblocking owner lock retain
// registration custody across its request and credential handoff. Every write
// syncs both the file and directory before its caller may use the new state.
// Descriptor-relative custody holds here as on Unix: leaf operations name the
// directory through its open handle's RootDirectory, and every open tolerates
// no reparse point where the Unix store tolerates no symlink.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const maximumRegistrationBytes = 32 * 1024

// Alternate leaf open modes, in place of the Unix store's flag arguments.
const (
	leafOpenRead = iota + 1
	leafOpenWriteCreate
)

// NT create dispositions and options this store hands to NtCreateFile. They
// are declared locally because the package's own NT option constants are
// incomplete on the create side.
const (
	ntDispositionOpen     = 0x00000001
	ntDispositionCreate   = 0x00000002
	ntDispositionOpenIf   = 0x00000003
	ntOptionDirectoryFile = 0x00000001
	ntOptionNonDirectory  = 0x00000040
	objDontReparse        = 0x00001000
	fileDeleteChildAccess = 0x00010000
)

// fileIDInfo has no host type in this x/sys version; the volume serial and the
// 128-bit file id together form the directory anchor, the Dev/Ino equivalent.
type fileIDInfo struct {
	VolumeSerialNumber uint64
	FileId             [16]byte
}

type custodyAnchor struct {
	volumeSerial uint64
	fileId       [16]byte
}

type registrationStore struct {
	directory *os.File
	lock      *os.File
	path      string
	client    string
	anchor    custodyAnchor
}

// Refuse namespace redirection before opening secrets or a durable request.
func openRegistrationStore(clientPath string) (_ *registrationStore, returnErr error) {
	if !filepath.IsAbs(clientPath) || filepath.Clean(clientPath) != clientPath || filepath.Dir(clientPath) == clientPath {
		return nil, errors.New("registration credential path is not canonical absolute")
	}
	dir := filepath.Dir(clientPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// The walk opens dir's every component by absolute path and refuses a
	// reparse point on each, the Windows parity of the Unix component walk
	// that refuses symlink components; the final handle is the custody
	// handle every leaf open anchors to through RootDirectory.
	directory, err := walkCustodyComponents(dir)
	if err != nil {
		return nil, err
	}
	self := &registrationStore{directory: directory, path: dir, client: filepath.Base(clientPath)}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, self.close())
		}
	}()
	if err := self.adoptAnchor(); err != nil {
		return nil, err
	}
	if err := self.check(); err != nil {
		return nil, err
	}
	self.lock, err = self.openLeaf(self.client+".registration.lock", windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE, ntDispositionOpenIf)
	if err != nil {
		return nil, err
	}
	if err := self.acquireOwnerLock(); err != nil {
		return nil, err
	}
	return self, nil
}

// Provider slots borrow the key owner's physical directory, never a fresh
// pathname selection. The handle remains tied to that directory identity
// across reads, writes and retries; check refuses a renamed or replaced
// visible namespace.
func openRegistrationStoreForOwner(clientPath string, owner *registrationStore) (_ *registrationStore, returnErr error) {
	if owner == nil || owner.directory == nil || owner.lock == nil || !filepath.IsAbs(clientPath) || filepath.Clean(clientPath) != clientPath || filepath.Dir(clientPath) != owner.path {
		return nil, errors.New("provider client custody differs from its retained directory owner")
	}
	if err := owner.check(); err != nil {
		return nil, err
	}
	// Refresh from the owner's own handle: the child anchors to that same
	// live directory without a second pathname selection.
	directory, err := selfChildOf(owner.directory)
	if err != nil {
		return nil, err
	}
	self := &registrationStore{directory: directory, path: owner.path, client: filepath.Base(clientPath), anchor: owner.anchor}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, self.close())
		}
	}()
	if err := self.check(); err != nil {
		return nil, err
	}
	self.lock, err = self.openLeaf(self.client+".registration.lock", windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE, ntDispositionOpenIf)
	if err != nil {
		return nil, err
	}
	if err := self.acquireOwnerLock(); err != nil {
		return nil, err
	}
	if err := self.check(); err != nil {
		return nil, err
	}
	return self, nil
}

// acquireOwnerLock takes the directory-wide exclusive owner lock over the lock
// handle's first byte without waiting; a lock already held anywhere else in
// the directory means the active owner keeps its registration custody.
func (self *registrationStore) acquireOwnerLock() error {
	if err := windows.LockFileEx(windows.Handle(self.lock.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{}); err != nil {
		return errRegistrationOwnerActive
	}
	return nil
}

// openLeaf opens one leaf through the directory handle and accepts it only on
// safe descriptor metadata, then applies the Unix no-symlink parity audit.
func (self *registrationStore) openLeaf(name string, access uint32, disposition uint32) (*os.File, error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return nil, errors.New("registration leaf is invalid")
	}
	file, err := ntOpenRelative(self.directory, name, access, disposition, ntOptionNonDirectory)
	if err != nil {
		return nil, err
	}
	if err := auditLeafCustody(file); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func (self *registrationStore) open(name string, flags int) (*os.File, error) {
	switch flags {
	case leafOpenRead:
		return self.openLeaf(name, windows.FILE_GENERIC_READ, ntDispositionOpen)
	case leafOpenWriteCreate:
		return self.openLeaf(name, windows.FILE_GENERIC_WRITE, ntDispositionCreate)
	default:
		return nil, errors.New("registration leaf open mode is unsupported")
	}
}

// Reopen checks only authenticate namespace identity; operations keep using
// the original handle even if a path is maliciously changed in between.
func (self *registrationStore) check() error {
	info, err := os.Lstat(self.path)
	if err != nil {
		return err
	}
	owned, err := self.directory.Stat()
	if err != nil {
		return err
	}
	current, err := self.anchorOf(windows.Handle(self.directory.Fd()))
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, owned) || current != self.anchor {
		return errors.New("registration custody directory identity changed")
	}
	if err := self.auditDirectoryCustody(); err != nil {
		return err
	}
	return nil
}

func (self *registrationStore) anchorOf(handle windows.Handle) (custodyAnchor, error) {
	var raw fileIDInfo
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileIdInfo, (*byte)(unsafe.Pointer(&raw)), uint32(unsafe.Sizeof(raw))); err != nil {
		return custodyAnchor{}, err
	}
	return custodyAnchor{volumeSerial: raw.VolumeSerialNumber, fileId: raw.FileId}, nil
}

// adoptAnchor takes the anchor at custody open: directory identity, ordinary
// (non reparse) kind, and one private owner.
func (self *registrationStore) adoptAnchor() error {
	var attributes uint32
	if err := openAttributes(windows.Handle(self.directory.Fd()), &attributes); err != nil {
		return err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("registration custody directory is a reparse point")
	}
	anchor, err := self.anchorOf(windows.Handle(self.directory.Fd()))
	if err != nil {
		return err
	}
	if err := self.auditDirectoryCustody(); err != nil {
		return err
	}
	self.anchor = anchor
	return nil
}

func (self *registrationStore) read(name string) ([]byte, error) {
	if err := self.check(); err != nil {
		return nil, err
	}
	file, err := self.open(name, leafOpenRead)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, maximumRegistrationBytes+1))
	closeErr := file.Close()
	if len(raw) > maximumRegistrationBytes {
		readErr = errors.Join(readErr, errors.New("registration data exceeds its finite bound"))
	}
	if err := errors.Join(readErr, closeErr, self.check()); err != nil {
		return nil, err
	}
	return raw, nil
}

// A single bounded census detects old identity files before first-key creation.
func (self *registrationStore) names(maximum int) ([]string, error) {
	if maximum < 1 || maximum > 4096 {
		return nil, errors.New("registration directory census bound differs")
	}
	if err := self.check(); err != nil {
		return nil, err
	}
	if _, err := self.directory.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	names, err := self.directory.Readdirnames(maximum + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(names) > maximum {
		return nil, errors.New("registration directory census exceeds its bound")
	}
	return names, self.check()
}

// Deletion uses the same owned directory as publication, by marking the leaf's
// own opened handle for deletion before its close. A missing leaf is already
// absent; a successful removal is durable before the caller proceeds.
func (self *registrationStore) remove(name string) error {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return errors.New("registration removal leaf is invalid")
	}
	if err := self.check(); err != nil {
		return err
	}
	file, err := ntOpenRelative(self.directory, name, windows.DELETE|windows.SYNCHRONIZE, ntDispositionOpen, ntOptionNonDirectory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return self.check()
		}
		return err
	}
	closeErr := markDeleteDisposition(file)
	if err := errors.Join(closeErr, file.Close()); err != nil {
		return err
	}
	return errors.Join(self.syncDirectory(), self.check())
}

// Rename publishes complete bytes; a failed directory sync remains an error
// and never licenses a second operation or a credential before durable handoff.
func (self *registrationStore) write(name string, raw []byte) (returnErr error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." || len(raw) == 0 || len(raw) > maximumRegistrationBytes {
		return errors.New("registration write exceeds its finite bound")
	}
	if err := self.check(); err != nil {
		return err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	temporary := ".registration-" + hex.EncodeToString(random[:])
	file, err := self.open(temporary, leafOpenWriteCreate)
	if err != nil {
		return err
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, self.remove(temporary))
		}
	}()
	_, writeErr := io.Copy(file, bytes.NewReader(raw))
	if err := errors.Join(writeErr, file.Sync(), file.Close(), self.check()); err != nil {
		return err
	}
	if err := self.renameLeaf(temporary, name); err != nil {
		return err
	}
	return errors.Join(self.syncDirectory(), self.check())
}

// renameLeaf publishes one same-directory rename. Publication moves through
// the pathname because the handle-rooted rename form needs the leaf open
// across the rename; custody stays descriptor-relative because every leaf
// operation routes through the directory handle and this rename only
// completes while the anchored directory identity is still live at the same
// pathname the walk verified. The delete disposition above keeps every other
// directory mutation on the handle.
func (self *registrationStore) renameLeaf(from, to string) error {
	if err := self.check(); err != nil {
		return err
	}
	source, err := windows.UTF16PtrFromString(filepath.Join(self.path, from))
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(filepath.Join(self.path, to))
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(source, target, windows.MOVEFILE_REPLACE_EXISTING); err != nil {
		return err
	}
	return self.check()
}

func (self *registrationStore) syncDirectory() error {
	return windows.FlushFileBuffers(windows.Handle(self.directory.Fd()))
}

// auditDirectoryCustody is the Windows parity of the Unix Mode&0022 bound: a
// directory owned by the process account, not writable by anyone outside its
// custody allowlist.
func (self *registrationStore) auditDirectoryCustody() error {
	return auditPrivateOwnership(windows.Handle(self.directory.Fd()), true,
		errors.New("registration parent must be owned and not writable by other users"))
}

// Releasing the lock and directory is part of the caller's completed result.
func (self *registrationStore) close() error {
	if self == nil {
		return nil
	}
	var err error
	if self.lock != nil {
		err = self.lock.Close()
		self.lock = nil
	}
	if self.directory != nil {
		err = errors.Join(err, self.directory.Close())
		self.directory = nil
	}
	return err
}

// openDirectoryNoReparse opens one absolute directory by pathname, refusing a
// reparse-backed tree, and returns a backup-semantics handle that admits at
// least one FlushFileBuffers of its own.
func openDirectoryNoReparse(path string) (*os.File, error) {
	w, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(w, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	var attributes uint32
	if err := openAttributes(handle, &attributes); err != nil {
		file.Close()
		return nil, err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, errors.Join(errors.New("registration custody directory is a reparse point"), file.Close())
	}
	return file, nil
}

// walkCustodyComponents opens dir's every component by absolute path, refusing
// a reparse-backed tree at each step; only the final handle survives.
func walkCustodyComponents(dir string) (*os.File, error) {
	volume := filepath.VolumeName(dir)
	components := strings.Split(strings.TrimPrefix(strings.TrimPrefix(dir, volume), string(filepath.Separator)), string(filepath.Separator))
	current, err := openDirectoryNoReparse(volume + string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	for _, component := range components {
		if component == "" {
			continue
		}
		next, err := openDirectoryNoReparse(filepath.Join(volume+string(filepath.Separator), component))
		closeErr := current.Close()
		if err != nil {
			return nil, errors.Join(err, closeErr)
		}
		current = next
	}
	return current, nil
}

// selfChildOf opens "." relative to the owner's own custody handle; the child
// anchors to the same live directory without another pathname selection.
func selfChildOf(parent *os.File) (*os.File, error) {
	file, err := ntOpenRelative(parent, ".", windows.GENERIC_READ|windows.GENERIC_WRITE, ntDispositionOpen, ntOptionDirectoryFile|windows.FILE_OPEN_FOR_BACKUP_INTENT)
	if err != nil {
		return nil, err
	}
	return file, nil
}

// ntOpenRelative opens one leaf through the parent's own RootDirectory with an
// object attribute that refuses reparse encounters from this root on. That is
// the Windows parity of Openat(O_NOFOLLOW), and it keeps every leaf operation
// bounded by the custody handle rather than by the visible pathname.
func ntOpenRelative(parent *os.File, name string, access uint32, disposition uint32, options uint32) (*os.File, error) {
	w, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(parent.Fd()),
		ObjectName:    w,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | objDontReparse,
	}
	var iosb windows.IO_STATUS_BLOCK
	var handle windows.Handle
	status := windows.NtCreateFile(&handle, access, oa, &iosb, nil, windows.FILE_ATTRIBUTE_NORMAL, 0, disposition, options, 0, 0)
	if status != nil {
		if nt, ok := status.(windows.NTStatus); ok {
			return nil, nt.Errno()
		}
		return nil, status
	}
	return os.NewFile(uintptr(handle), name), nil
}

func openAttributes(handle windows.Handle, attributes *uint32) error {
	var basic fileBasicInfoFields
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))); err != nil {
		return err
	}
	*attributes = basic.FileAttributes
	return nil
}

type fileBasicInfoFields struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
	reserved       uint32
}

// lastWriteNanos returns the file's own last-write time, the Windows parity of
// the Unix fingerprint's modification timestamp.
func lastWriteNanos(handle windows.Handle) (int64, error) {
	var basic fileBasicInfoFields
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))); err != nil {
		return 0, err
	}
	filetime := windows.Filetime{
		LowDateTime:  uint32(basic.LastWriteTime),
		HighDateTime: uint32(basic.LastWriteTime >> 32),
	}
	return filetime.Nanoseconds(), nil
}

// markDeleteDisposition marks an open leaf's handle for deletion; its close
// completes the removal without walking a pathname.
func markDeleteDisposition(file *os.File) error {
	var info struct{ DeleteFile uint32 }
	info.DeleteFile = 1
	iosb := &windows.IO_STATUS_BLOCK{}
	return windows.NtSetInformationFile(windows.Handle(file.Fd()), iosb, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), windows.FileDispositionInfo)
}

// auditLeafCustody is the Windows parity of the Unix leaf stat bound: a
// private, single-linked, bounded regular file owned by the process account,
// readable and writable by nobody outside its custody allowlist.
func auditLeafCustody(file *os.File) error {
	handle := windows.Handle(file.Fd())
	var bh windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &bh); err != nil {
		return err
	}
	size := int64(bh.FileSizeHigh)<<32 | int64(bh.FileSizeLow)
	if bh.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || bh.NumberOfLinks != 1 || size > maximumRegistrationBytes || size < 0 {
		return errors.New("registration file has unsafe ownership, mode, links or size")
	}
	if err := auditPrivateOwnership(handle, false,
		errors.New("registration file has unsafe ownership, mode, links or size")); err != nil {
		return err
	}
	return nil
}

// auditPrivateOwnership refuses an owner that is not the process account and
// any allowlist ACE granting the audited data access to a SID outside
// {owner account, SYSTEM, Administrators}. wholeDirectory audits writes only,
// the Mode&0022 parity; leaves audit reads and writes, the 0600 parity.
func auditPrivateOwnership(handle windows.Handle, wholeDirectory bool, refusal error) error {
	self, err := custodySelfSid()
	if err != nil {
		return refusal
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if owner == nil || !windows.EqualSid(owner, self) {
		return refusal
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return nil
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return err
		}
		if ace == nil {
			continue
		}
		const leafAuditMask = windows.FILE_READ_DATA | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA
		const directoryAuditMask = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | fileDeleteChildAccess
		granted := uint32(ace.Mask) & directoryAuditMask
		if !wholeDirectory {
			granted = uint32(ace.Mask) & leafAuditMask
		}
		if granted == 0 || !custodySidOutside((*windows.SID)(unsafe.Pointer(&ace.SidStart)), self) {
			continue
		}
		return refusal
	}
	return nil
}

var custodyAccounts struct {
	once           sync.Once
	processSid     *windows.SID
	systemSid      *windows.SID
	administrators *windows.SID
	ready          bool
}

// custodySelfSid resolves the process token's user account once; the two
// known-system integrity accounts resolve beside it as the custody allowlist.
// A failed resolution keeps custody refusing rather than loosening.
func custodySelfSid() (*windows.SID, error) {
	custodyAccounts.once.Do(loadCustodyAccounts)
	if !custodyAccounts.ready || custodyAccounts.processSid == nil {
		return nil, errors.New("registration account identity is unavailable")
	}
	return custodyAccounts.processSid, nil
}

func loadCustodyAccounts() {
	token := windows.GetCurrentProcessToken()
	if user, err := token.GetTokenUser(); err == nil && user.User.Sid != nil {
		custodyAccounts.processSid = user.User.Sid
	}
	custodyAccounts.systemSid, _ = windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	custodyAccounts.administrators, _ = windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	custodyAccounts.ready = custodyAccounts.processSid != nil && custodyAccounts.systemSid != nil && custodyAccounts.administrators != nil
}

// custodySidOutside reports whether an ACE's SID is nobody's account this
// custody lets in: only the owner account, SYSTEM and Administrators.
func custodySidOutside(sid *windows.SID, self *windows.SID) bool {
	if windows.EqualSid(sid, self) {
		return false
	}
	if custodyAccounts.systemSid != nil && windows.EqualSid(sid, custodyAccounts.systemSid) {
		return false
	}
	if custodyAccounts.administrators != nil && windows.EqualSid(sid, custodyAccounts.administrators) {
		return false
	}
	return true
}
