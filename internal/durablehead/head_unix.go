//go:build linux || darwin

// Snapshots publish through the retained directory: a create marker names the
// temporary before it exists, the pending head is anchored before the bytes so a
// prepared record whose bytes reached stable storage survives an interrupted
// commit, and an exchanged predecessor remains present until both physical
// generations have been checked.
package durablehead

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/urfoundation/sn/internal/durablepath"
	"github.com/urnetwork/connect/durablevolume"
	"golang.org/x/sys/unix"

	"github.com/urnetwork/connect/durablesys"
)

// Methods serialize their state. The caller owns the borrowed directory and
// writer lock, joins all users, closes this owner, then releases those objects.
// No method silently reacquires a different directory/lock generation.
type Owner struct {
	stateLock     sync.Mutex
	ctx           context.Context
	directory     *durablepath.Directory
	lock          *os.File
	spec          Spec
	checkpoint    Checkpoint
	checkpointRaw []byte
	knownKVs      map[string]unix.Stat_t
	failure       error
	closed        bool
	readOnly      bool
	at            func(string) error
}

// Normal admission refuses every pending operation. The same borrowed lock
// description already held by the application avoids a second flock owner.
func Open(ctx context.Context, directory *durablepath.Directory, lock *os.File, spec Spec) (*Owner, error) {
	return open(ctx, directory, lock, spec, false, false)
}

// Passive readers share the application's preprovisioned marker lock. The
// borrowed descriptor retains a shared lease until its caller closes it. An
// unresolved pending head is refused; no read path finishes publication.
func OpenReadOnly(ctx context.Context, directory *durablepath.Directory, lock *os.File, spec Spec) (*Owner, error) {
	return open(ctx, directory, lock, spec, false, true)
}

// The caller first joins/closes its old owner. Only complete retained next
// bytes can finish; absent, partial, extra or unknown bytes remain refused.
func Reconcile(ctx context.Context, directory *durablepath.Directory, lock *os.File, spec Spec) (*Owner, error) {
	return open(ctx, directory, lock, spec, true, false)
}

func open(ctx context.Context, directory *durablepath.Directory, lock *os.File, spec Spec, reconcile, readOnly bool) (*Owner, error) {
	if ctx == nil || directory == nil || directory.File() == nil || lock == nil {
		return nil, errors.New("snapshot owner context, directory or writer lock is absent")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	spec.AuxiliaryNames = append([]string(nil), spec.AuxiliaryNames...)
	sort.Strings(spec.AuxiliaryNames)
	self := &Owner{ctx: ctx, directory: directory, lock: lock, spec: spec, knownKVs: map[string]unix.Stat_t{}, readOnly: readOnly}
	if err := self.admit(false); err != nil {
		return nil, err
	}
	lockMode := unix.LOCK_EX
	if readOnly {
		lockMode = unix.LOCK_SH
	}
	if err := unix.Flock(int(lock.Fd()), lockMode|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errors.Join(durablevolume.ErrBusy, err)
		}
		return nil, observation(err, false)
	}
	raw, err := self.readCheckpoint()
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&self.checkpoint); err != nil {
		return nil, identity(err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, identity(errors.New("snapshot checkpoint has trailing data"))
	}
	// Canonical bytes also reject duplicate keys and ambiguous provisioning.
	canonical, err := json.Marshal(self.checkpoint)
	if err != nil || !bytes.Equal(raw, canonical) {
		return nil, identity(errors.New("snapshot checkpoint is not canonical"), err)
	}
	self.checkpointRaw = raw
	if err := self.validateCheckpoint(); err != nil {
		return nil, err
	}
	if err := self.checkAuxiliaries(); err != nil {
		return nil, err
	}
	if err := self.checkTemporaryCensus(reconcile); err != nil {
		return nil, err
	}
	if self.checkpoint.Pending != nil {
		if !reconcile {
			return nil, ErrUncertain
		}
		if err := self.reconcile(); err != nil {
			return nil, err
		}
	}
	if _, err := self.readMember(self.spec.Name, self.checkpoint.Committed); err != nil {
		return nil, err
	}
	if err := self.check(false); err != nil {
		return nil, err
	}
	return self, nil
}

func validateSpec(spec Spec) error {
	if len(spec.Kind) < 1 || len(spec.Kind) > 64 || !simpleName(spec.Name) || spec.MaximumBytes < 1 || spec.MaximumBytes > 64<<20 || len(spec.AuxiliaryNames) > 8 {
		return errors.New("snapshot specification is invalid or unbounded")
	}
	for _, c := range spec.Kind {
		if c != '-' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return errors.New("snapshot kind is invalid")
		}
	}
	seen := map[string]bool{spec.Name: true}
	for _, name := range spec.AuxiliaryNames {
		if !simpleName(name) || seen[name] {
			return errors.New("snapshot auxiliary names are invalid or duplicate")
		}
		seen[name] = true
	}
	if spec.LockName != "" && (!seen[spec.LockName] || spec.LockName == spec.Name) {
		return errors.New("snapshot lock must be a preprovisioned auxiliary")
	}
	return nil
}

func simpleName(name string) bool {
	return name != "" && len(name) <= 160 && name != "." && name != ".." && filepath.Base(name) == name && !strings.ContainsRune(name, 0) && !strings.HasPrefix(name, ".durable-head-")
}

func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func identity(errs ...error) error {
	return errors.Join(append([]error{durablevolume.ErrIdentity}, errs...)...)
}

// Successfully observed disappearance/alias is loss; a failed observation is
// retryable. Invalid borrowed descriptors remain caller errors.
func observation(err error, expected bool) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EBADF) {
		return err
	}
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) || (expected && errors.Is(err, unix.ENOENT)) {
		return identity(err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.Join(durablevolume.ErrUnavailable, err)
}

func (self *Owner) retain(err error) error {
	if errors.Is(err, durablevolume.ErrIdentity) || errors.Is(err, ErrUncertain) {
		self.failure = errors.Join(self.failure, err)
	}
	return err
}

func (self *Owner) boundary(name string) error {
	if self.at == nil {
		return nil
	}
	return self.at(name)
}

// Caller context and physical ownership are independent authorities.
func (self *Owner) admit(write bool) error {
	if self.closed {
		return errors.New("snapshot owner is closed")
	}
	if self.failure != nil {
		return self.failure
	}
	if err := self.ctx.Err(); err != nil {
		return err
	}
	if write && self.readOnly {
		return ErrReadOnly
	}
	var err error
	if write {
		err = self.directory.CheckWrite()
	} else {
		err = self.directory.CheckRead()
	}
	if err != nil {
		return self.retain(err)
	}
	var opened, named unix.Stat_t
	if err := unix.Fstat(int(self.lock.Fd()), &opened); err != nil {
		return observation(err, false)
	}
	if self.spec.LockName == "" {
		if err := unix.Fstat(int(self.directory.File().Fd()), &named); err != nil {
			return observation(err, false)
		}
	} else {
		if err := unix.Fstatat(int(self.directory.File().Fd()), self.spec.LockName, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return self.retain(observation(err, true))
		}
	}
	if opened.Dev != named.Dev || opened.Ino != named.Ino || opened.Mode != named.Mode || (self.spec.LockName != "" && (named.Mode&unix.S_IFMT != unix.S_IFREG || named.Mode&0077 != 0 || named.Nlink != 1)) {
		return self.retain(identity(errors.New("snapshot writer lock changed")))
	}
	return self.ctx.Err()
}

func (self *Owner) readCheckpoint() ([]byte, error) {
	if err := self.admit(false); err != nil {
		return nil, err
	}
	if err := self.boundary("checkpoint-observe"); err != nil {
		return nil, observation(err, false)
	}
	raw := make([]byte, 4096)
	n, err := durablesys.GetAttribute(int(self.attributeFile().Fd()), Attribute(self.spec.Kind, self.spec.Name), raw)
	if err != nil {
		if errors.Is(err, durablesys.ErrNoAttribute) || errors.Is(err, unix.ERANGE) {
			return nil, self.retain(identity(errors.New("snapshot checkpoint is absent or oversized"), err))
		}
		return nil, observation(err, false)
	}
	return raw[:n], self.admit(false)
}

// Independent file-lock owners do not share one directory's finite xattr area.
// The auxiliary checkpoint binds the exact lock inode; no absent-file fallback.
func (self *Owner) attributeFile() *os.File {
	if self.spec.LockName != "" {
		return self.lock
	}
	return self.directory.File()
}

func (self *Owner) validateCheckpoint() error {
	c := &self.checkpoint
	var stat unix.Stat_t
	if err := unix.Fstat(int(self.directory.File().Fd()), &stat); err != nil {
		return observation(err, false)
	}
	if c.Schema != Schema || c.Kind != self.spec.Kind || c.Name != self.spec.Name || c.MaximumBytes != self.spec.MaximumBytes || c.DirectoryInode != stat.Ino || c.LockName != self.spec.LockName || len(c.Auxiliaries) != len(self.spec.AuxiliaryNames) {
		return identity(errors.New("snapshot checkpoint differs from its declared owner"))
	}
	for i, name := range self.spec.AuxiliaryNames {
		if c.Auxiliaries[i].Name != name || c.Auxiliaries[i].Inode == 0 {
			return identity(errors.New("snapshot auxiliary authority differs"))
		}
	}
	if !validMember(c.Committed, self.spec.MaximumBytes) {
		return identity(errors.New("snapshot committed head is invalid"))
	}
	if c.Pending != nil && (!self.temporaryName(c.Pending.Temporary) || !c.Pending.Next.Present || !validMember(c.Pending.Next, self.spec.MaximumBytes) || c.Pending.Next.Inode == c.Committed.Inode) {
		return identity(errors.New("snapshot pending head is invalid"))
	}
	return nil
}

func validMember(member Member, maximum int64) bool {
	if !member.Present {
		return member == (Member{})
	}
	raw, err := hex.DecodeString(member.Sha256)
	return member.Inode != 0 && member.Size >= 0 && member.Size <= maximum && err == nil && len(raw) == sha256.Size && strings.ToLower(member.Sha256) == member.Sha256
}

func sameStat(left, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino && left.Mode == right.Mode && left.Uid == right.Uid && left.Nlink == right.Nlink && left.Size == right.Size && left.Mtim == right.Mtim && left.Ctim == right.Ctim
}

func (self *Owner) stat(name string, expected bool) (unix.Stat_t, bool, error) {
	var stat unix.Stat_t
	if err := self.boundary("named-observe:" + name); err != nil {
		return stat, false, self.retain(observation(err, expected))
	}
	err := unix.Fstatat(int(self.directory.File().Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) && !expected {
		return stat, false, nil
	}
	if err != nil {
		return stat, false, self.retain(observation(err, expected))
	}
	var root unix.Stat_t
	if err := unix.Fstat(int(self.directory.File().Fd()), &root); err != nil {
		return stat, false, observation(err, false)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0077 != 0 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || stat.Dev != root.Dev {
		return stat, false, self.retain(identity(errors.New("snapshot member is not one private regular file on its volume")))
	}
	return stat, true, nil
}

func (self *Owner) sameNamed(file *os.File, name string, expected *unix.Stat_t) (unix.Stat_t, error) {
	var opened unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &opened); err != nil {
		return opened, observation(err, false)
	}
	named, _, err := self.stat(name, true)
	if err != nil {
		return opened, err
	}
	if !sameStat(opened, named) || (expected != nil && !sameStat(opened, *expected)) {
		return opened, self.retain(identity(errors.New("snapshot named file changed during custody")))
	}
	return opened, nil
}

func (self *Owner) checkAuxiliaries() error {
	for _, member := range self.checkpoint.Auxiliaries {
		stat, _, err := self.stat(member.Name, true)
		if err != nil {
			return err
		}
		if stat.Ino != member.Inode {
			return self.retain(identity(errors.New("snapshot auxiliary inode changed")))
		}
		if known, present := self.knownKVs[member.Name]; present && !sameStat(stat, known) {
			return self.retain(identity(errors.New("snapshot auxiliary changed outside its owner")))
		}
		self.knownKVs[member.Name] = stat
	}
	return nil
}

func (self *Owner) readMember(name string, member Member) ([]byte, error) {
	if err := self.admit(false); err != nil {
		return nil, err
	}
	before, present, err := self.stat(name, member.Present)
	if err != nil {
		return nil, err
	}
	if !member.Present {
		if present {
			return nil, self.retain(identity(errors.New("unacknowledged snapshot appeared")))
		}
		return nil, nil
	}
	if before.Ino != member.Inode || before.Size != member.Size {
		return nil, self.retain(identity(errors.New("snapshot physical head differs")))
	}
	fd, err := unix.Openat(int(self.directory.File().Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, self.retain(observation(err, true))
	}
	file := os.NewFile(uintptr(fd), name)
	if _, err := self.sameNamed(file, name, &before); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err := self.boundary("read-opened"); err != nil {
		return nil, errors.Join(observation(err, false), file.Close())
	}
	raw, readErr := readBounded(self.ctx, file, self.spec.MaximumBytes)
	_, postErr := self.sameNamed(file, name, &before)
	closeErr := file.Close()
	if err := errors.Join(observation(readErr, false), postErr, observation(closeErr, false), self.admit(false)); err != nil {
		return nil, self.retain(err)
	}
	if int64(len(raw)) != member.Size || digest(raw) != member.Sha256 {
		return nil, self.retain(identity(errors.New("snapshot bytes differ from the acknowledged head")))
	}
	self.knownKVs[name] = before
	return raw, nil
}

// Hash scans obey finite bytes and context between each bounded read.
func readBounded(ctx context.Context, file *os.File, maximum int64) ([]byte, error) {
	var buffer bytes.Buffer
	chunk := make([]byte, 32<<10)
	for int64(buffer.Len()) <= maximum {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		limit := min(int64(len(chunk)), maximum+1-int64(buffer.Len()))
		n, err := file.Read(chunk[:limit])
		buffer.Write(chunk[:n])
		if errors.Is(err, io.EOF) {
			return buffer.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
	return nil, identity(errors.New("snapshot read exceeded capacity"))
}

func (self *Owner) prefix() string {
	return ".durable-head-" + digest([]byte(self.spec.Kind + "\x00" + self.spec.Name))[:32] + "-"
}
func (self *Owner) temporaryName(name string) bool {
	if !strings.HasPrefix(name, self.prefix()) || len(name) != len(self.prefix())+32 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(name, self.prefix()))
	return err == nil
}

// A temporary is refused unless a create marker names it. A marker that names
// no existing member is stale and safe to clear in either mode; a marker pair
// with no pending checkpoint is cleared only by Reconcile. A prefixed member
// that is neither shape is unknown and stays refused.
func (self *Owner) checkTemporaryCensus(allowReconcile bool) (resultErr error) {
	fd, err := unix.Openat(int(self.directory.File().Fd()), ".", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
	if err != nil {
		return observation(err, false)
	}
	file := os.NewFile(uintptr(fd), self.directory.File().Name())
	defer func() { resultErr = errors.Join(resultErr, observation(file.Close(), false)) }()
	count := 0
	temporaries := map[string]bool{}
	markers := map[string]string{}
	unknown := ""
	for {
		if err := self.ctx.Err(); err != nil {
			return err
		}
		names, err := file.Readdirnames(64)
		count += len(names)
		if count > 10000 {
			return errors.Join(durablevolume.ErrUnavailable, errors.New("snapshot namespace exceeds finite member capacity"))
		}
		for _, name := range names {
			rest, ok := strings.CutPrefix(name, self.prefix())
			if !ok {
				continue
			}
			if nonce, isMarker := strings.CutPrefix(rest, "create-"); isMarker {
				if len(nonce) == 32 {
					if _, err := hex.DecodeString(nonce); err == nil {
						markers[name] = nonce
						continue
					}
				}
			}
			if len(rest) == 32 {
				if _, err := hex.DecodeString(rest); err == nil {
					temporaries[name] = true
					continue
				}
			}
			unknown = name
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return observation(err, false)
		}
	}
	if unknown != "" {
		return errors.Join(ErrUncertain, fmt.Errorf("unrecognized snapshot member %q is retained", unknown))
	}
	removed := false
	for marker, nonce := range markers {
		temporary := self.prefix() + nonce
		pendingNames := self.checkpoint.Pending != nil && self.checkpoint.Pending.Temporary == temporary
		if !temporaries[temporary] {
			if pendingNames {
				// The member may already be renamed into place; only then can
				// finishPending complete without the temporary.
				if renamed, err := self.matches(self.spec.Name, self.checkpoint.Pending.Next); err != nil {
					return err
				} else if renamed {
					continue
				}
				// The checkpoint names it but its bytes are gone: keep the
				// marker as evidence and refuse rather than clear anything.
				return errors.Join(ErrUncertain, errors.New("pending snapshot temporary is missing"))
			}
			// It names no member, so clearing it is safe in either mode.
			if err := unix.Unlinkat(int(self.directory.File().Fd()), marker, 0); err != nil {
				return errors.Join(ErrUncertain, observation(err, false))
			}
			removed = true
			continue
		}
		if pendingNames {
			complete, err := self.matches(temporary, self.checkpoint.Pending.Next)
			if err != nil {
				return err
			}
			if !complete {
				// An exchange rename leaves the next bytes at the name and the
				// previous committed bytes at the temporary; that is complete.
				if renamed, err := self.matches(self.spec.Name, self.checkpoint.Pending.Next); err != nil {
					return err
				} else if renamed {
					if committed, err := self.matches(temporary, self.checkpoint.Committed); err != nil {
						return err
					} else if committed {
						complete = true
					}
				}
			}
			if complete {
				// The prepared bytes reached stable storage: finishPending keeps
				// the exact signed record.
				continue
			}
			stat, present, err := self.stat(temporary, false)
			if err != nil {
				return err
			}
			if !present || stat.Size != 0 {
				// Bytes were written but not completely: that is custody loss or
				// tampering, never a publication that never started. Refuse.
				return errors.Join(ErrUncertain, errors.New("incomplete pending snapshot is retained"))
			}
			if !allowReconcile {
				return errors.Join(ErrUncertain, errors.New("incomplete pending snapshot is retained"))
			}
			// The anchored publication never wrote a byte, so no prepared record
			// exists. Abandon it; the committed generation is authoritative.
			if err := unix.Unlinkat(int(self.directory.File().Fd()), temporary, 0); err != nil {
				return errors.Join(ErrUncertain, observation(err, false))
			}
			if err := self.directory.File().Sync(); err != nil {
				return errors.Join(ErrUncertain, observation(err, false))
			}
			if err := self.abandonPending(); err != nil {
				return err
			}
			delete(temporaries, temporary)
			if err := unix.Unlinkat(int(self.directory.File().Fd()), marker, 0); err != nil {
				return errors.Join(ErrUncertain, observation(err, false))
			}
			if err := self.directory.File().Sync(); err != nil {
				return errors.Join(ErrUncertain, observation(err, false))
			}
			removed = true
			continue
		}
		if !allowReconcile {
			return errors.Join(ErrUncertain, errors.New("snapshot create marker is retained"))
		}
		if err := unix.Unlinkat(int(self.directory.File().Fd()), temporary, 0); err != nil {
			return errors.Join(ErrUncertain, observation(err, false))
		}
		// Persist the removal before the marker goes, so a crash can never
		// leave a markerless temporary.
		if err := self.directory.File().Sync(); err != nil {
			return errors.Join(ErrUncertain, observation(err, false))
		}
		delete(temporaries, temporary)
		if err := unix.Unlinkat(int(self.directory.File().Fd()), marker, 0); err != nil {
			return errors.Join(ErrUncertain, observation(err, false))
		}
		removed = true
	}
	for name := range temporaries {
		if self.checkpoint.Pending != nil && name == self.checkpoint.Pending.Temporary {
			continue
		}
		return errors.Join(ErrUncertain, errors.New("unanchored snapshot temporary is retained"))
	}
	if removed {
		if err := self.directory.File().Sync(); err != nil {
			return errors.Join(ErrUncertain, observation(err, false))
		}
	}
	return nil
}

// abandonPending clears an anchored publication whose bytes never completed.
// Only Reconcile calls it, and the committed snapshot is unchanged.
func (self *Owner) abandonPending() error {
	if self.checkpoint.Pending == nil {
		return nil
	}
	next := self.checkpoint
	next.Pending = nil
	return self.writeCheckpoint(next, "abandon")
}

// writeCreateMarker names the temporary before it exists, so a process killed
// between the two leaves a recoverable pair instead of an unknown file.
func (self *Owner) writeCreateMarker(nonce string) error {
	name := self.prefix() + "create-" + nonce
	fd, err := unix.Openat(int(self.directory.File().Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return observation(err, false)
	}
	file := os.NewFile(uintptr(fd), name)
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return observation(err, false)
	}
	return self.directory.File().Sync()
}

// removeCreateMarker is best effort after the checkpoint names the temporary;
// a leftover marker is cleared by Reconcile.
func (self *Owner) removeCreateMarker(nonce string) error {
	name := self.prefix() + "create-" + nonce
	if err := unix.Unlinkat(int(self.directory.File().Fd()), name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return observation(err, false)
	}
	return self.directory.File().Sync()
}

func (self *Owner) check(write bool) error {
	if err := self.admit(write); err != nil {
		return err
	}
	raw, err := self.readCheckpoint()
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, self.checkpointRaw) {
		return self.retain(identity(errors.New("snapshot checkpoint changed outside its owner")))
	}
	if self.checkpoint.Pending != nil {
		return ErrUncertain
	}
	if err := self.checkAuxiliaries(); err != nil {
		return err
	}
	stat, present, err := self.stat(self.spec.Name, self.checkpoint.Committed.Present)
	if err != nil {
		return err
	}
	if present != self.checkpoint.Committed.Present {
		return self.retain(identity(errors.New("snapshot member presence changed")))
	}
	if present {
		known, ok := self.knownKVs[self.spec.Name]
		if !ok || !sameStat(stat, known) {
			return self.retain(identity(errors.New("snapshot changed after its head was admitted")))
		}
	}
	return self.admit(write)
}

// Reads retain physical identity without requiring unused write reserve.
func (self *Owner) Check() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.check(false)
}

func (self *Owner) CheckWrite() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.check(true)
}

// A false presence is possible only under an explicit preprovisioned empty head.
func (self *Owner) Read() ([]byte, bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err := self.check(false); err != nil {
		return nil, false, err
	}
	raw, err := self.readMember(self.spec.Name, self.checkpoint.Committed)
	return raw, self.checkpoint.Committed.Present, err
}

// Only preprovisioned pinned marker inodes can be opened. The callback must not
// close/retain the borrowed file. Real sync and named-file postchecks always run.
func (self *Owner) WithAuxiliary(name string, write bool, action func(*os.File) error) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if action == nil {
		return errors.New("snapshot auxiliary operation is absent")
	}
	if _, present := self.knownKVs[name]; !present || name == self.spec.Name {
		return errors.New("snapshot auxiliary is not declared")
	}
	if err := self.check(write); err != nil {
		return err
	}
	flags := unix.O_RDONLY
	if write {
		flags = unix.O_RDWR
	}
	fd, err := unix.Openat(int(self.directory.File().Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return self.retain(observation(err, true))
	}
	file := os.NewFile(uintptr(fd), name)
	before := self.knownKVs[name]
	if _, err := self.sameNamed(file, name, &before); err != nil {
		return errors.Join(err, file.Close())
	}
	callErr := action(file)
	if write {
		callErr = errors.Join(callErr, file.Sync())
	}
	var expected *unix.Stat_t
	if !write {
		expected = &before
	}
	after, postErr := self.sameNamed(file, name, expected)
	err = errors.Join(callErr, postErr, file.Close(), self.admit(write))
	if err != nil {
		if write {
			return self.retain(errors.Join(ErrUncertain, err))
		}
		return self.retain(observation(err, false))
	}
	self.knownKVs[name] = after
	return nil
}

// Releasing this state does not release the application's borrowed descriptors.
func (self *Owner) Close() error {
	if self == nil {
		return nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closed = true
	return nil
}

func (self *Owner) writeCheckpoint(next Checkpoint, boundary string) error {
	if err := self.admit(true); err != nil {
		return err
	}
	actual, err := self.readCheckpoint()
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, self.checkpointRaw) {
		return self.retain(identity(errors.New("snapshot checkpoint changed before publication")))
	}
	raw, err := json.Marshal(next)
	if err != nil || len(raw) > 4096 {
		return errors.Join(errors.New("snapshot checkpoint exceeds capacity"), err)
	}
	if err := durablesys.SetAttribute(int(self.attributeFile().Fd()), Attribute(self.spec.Kind, self.spec.Name), raw, durablesys.AttributeReplace); err != nil {
		return errors.Join(ErrUncertain, observation(err, false))
	}
	if self.spec.LockName != "" {
		// Our xattr update legitimately changes ctime, but cannot authorize a
		// simultaneous change of marker bytes, permissions or named inode.
		before := self.knownKVs[self.spec.LockName]
		after, err := self.sameNamed(self.lock, self.spec.LockName, nil)
		if err != nil {
			return errors.Join(ErrUncertain, err)
		}
		compared := after
		compared.Ctim = before.Ctim
		if !sameStat(compared, before) {
			return errors.Join(ErrUncertain, identity(errors.New("snapshot marker changed during checkpoint update")))
		}
		self.knownKVs[self.spec.LockName] = after
	}
	if err := self.boundary(boundary + "-written"); err != nil {
		return errors.Join(ErrUncertain, err)
	}
	if err := self.attributeFile().Sync(); err != nil {
		return errors.Join(ErrUncertain, err)
	}
	if self.spec.LockName != "" {
		if err := self.directory.File().Sync(); err != nil {
			return errors.Join(ErrUncertain, err)
		}
	}
	if err := self.boundary(boundary + "-synced"); err != nil {
		return errors.Join(ErrUncertain, err)
	}
	actual, err = self.readCheckpoint()
	if err != nil {
		// A failed acknowledgment read retains the pending write, but cannot
		// establish different bytes or invalidate the original custody.
		return errors.Join(ErrUncertain, err)
	}
	if !bytes.Equal(actual, raw) {
		return errors.Join(ErrUncertain, identityIfDifferent(actual, raw))
	}
	self.checkpoint, self.checkpointRaw = next, raw
	return self.admit(true)
}

func identityIfDifferent(left, right []byte) error {
	if !bytes.Equal(left, right) {
		return identity(errors.New("snapshot checkpoint acknowledgement differs"))
	}
	return nil
}

// The application supplies exact serialized bytes, including any prior signed
// records. The optional hook runs AFTER the real directory sync and cannot
// replace it. A failed hook is treated as a lost acknowledgement, not success.
func (self *Owner) Publish(raw []byte, afterSync func(*os.File) error) (resultErr error) {
	return self.PublishWithHooks(raw, PublicationHooks{AfterDirectorySync: afterSync})
}

// Application observers run inside this serialized operation, after real I/O.
func (self *Owner) PublishWithHooks(raw []byte, hooks PublicationHooks) (resultErr error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if int64(len(raw)) > self.spec.MaximumBytes {
		return errors.Join(durablevolume.ErrUnavailable, errors.New("snapshot payload exceeds declared capacity"))
	}
	if err := self.check(true); err != nil {
		return err
	}
	if err := self.checkTemporaryCensus(false); err != nil {
		return self.retain(err)
	}
	if err := self.ctx.Err(); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temporary := self.prefix() + hex.EncodeToString(nonce[:])
	nonceHex := hex.EncodeToString(nonce[:])
	if err := self.writeCreateMarker(nonceHex); err != nil {
		return err
	}
	fd, err := unix.Openat(int(self.directory.File().Fd()), temporary, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return observation(err, false)
	}
	file := os.NewFile(uintptr(fd), temporary)
	defer func() {
		if file != nil {
			resultErr = errors.Join(resultErr, file.Close())
		}
		if resultErr != nil {
			resultErr = self.retain(errors.Join(ErrUncertain, resultErr))
		}
	}()
	if err := self.boundary("temporary-created"); err != nil {
		return err
	}
	// The pending checkpoint is anchored before the bytes, so a prepared record
	// whose bytes reached stable storage is never discarded by a later barrier.
	// Reconcile abandons an anchored publication whose bytes never completed.
	stat, _, err := self.stat(temporary, true)
	if err != nil {
		return err
	}
	next := Member{Present: true, Inode: stat.Ino, Size: int64(len(raw)), Sha256: digest(raw)}
	pending := self.checkpoint
	pending.Pending = &Pending{Temporary: temporary, Next: next}
	if err := self.writeCheckpoint(pending, "pending"); err != nil {
		return err
	}
	if err := self.admit(true); err != nil {
		return err
	}
	n, writeErr := file.Write(raw)
	if n != len(raw) && writeErr == nil {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		return writeErr
	}
	if err := self.boundary("temporary-written"); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := self.boundary("temporary-synced"); err != nil {
		return err
	}
	if hooks.AfterFileSync != nil {
		if err := hooks.AfterFileSync(); err != nil {
			return err
		}
	}
	if _, err := self.sameNamed(file, temporary, nil); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		file = nil
		return err
	}
	file = nil
	if _, err := self.readMember(temporary, next); err != nil {
		return err
	}
	if err := self.finishPending(hooks); err != nil {
		return err
	}
	if err := self.removeCreateMarker(nonceHex); err != nil {
		return err
	}
	return nil
}

// One exact next generation, before or after exchange, is the only recovery.
func (self *Owner) reconcile() error {
	if err := self.admit(true); err != nil {
		return err
	}
	pending := self.checkpoint.Pending
	if err := self.finishPending(PublicationHooks{}); err != nil {
		return self.retain(errors.Join(ErrUncertain, err))
	}
	if pending != nil {
		if err := self.removeCreateMarker(strings.TrimPrefix(pending.Temporary, self.prefix())); err != nil {
			return self.retain(errors.Join(ErrUncertain, err))
		}
	}
	return nil
}

func (self *Owner) matches(name string, member Member) (bool, error) {
	stat, present, err := self.stat(name, false)
	if err != nil {
		return false, err
	}
	if !member.Present {
		return !present, nil
	}
	if !present || stat.Ino != member.Inode || stat.Size != member.Size {
		return false, nil
	}
	_, err = self.readMember(name, member)
	return err == nil, err
}

func (self *Owner) syncMember(name string, member Member) error {
	if !member.Present {
		return nil
	}
	if _, err := self.readMember(name, member); err != nil {
		return err
	}
	fd, err := unix.Openat(int(self.directory.File().Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return observation(err, true)
	}
	file := os.NewFile(uintptr(fd), name)
	before := self.knownKVs[name]
	_, preErr := self.sameNamed(file, name, &before)
	if preErr != nil {
		return errors.Join(preErr, file.Close())
	}
	syncErr := file.Sync()
	_, postErr := self.sameNamed(file, name, &before)
	return errors.Join(syncErr, postErr, file.Close(), self.admit(true))
}

func (self *Owner) finishPending(hooks PublicationHooks) error {
	pending := self.checkpoint.Pending
	if pending == nil {
		return errors.New("snapshot has no pending operation")
	}
	if err := self.checkAuxiliaries(); err != nil {
		return err
	}
	if err := self.checkTemporaryCensus(false); err != nil {
		return err
	}
	finalNext, err := self.matches(self.spec.Name, pending.Next)
	if err != nil {
		return err
	}
	if !finalNext {
		finalOld, err := self.matches(self.spec.Name, self.checkpoint.Committed)
		if err != nil {
			return err
		}
		temporaryNext, err := self.matches(pending.Temporary, pending.Next)
		if err != nil {
			return err
		}
		if !finalOld || !temporaryNext {
			return identity(errors.New("snapshot pending bytes are partial, missing or unknown"))
		}
		if err := self.syncMember(pending.Temporary, pending.Next); err != nil {
			return err
		}
		if err := self.admit(true); err != nil {
			return err
		}
		rename := durablesys.RenameNoReplace
		if self.checkpoint.Committed.Present {
			rename = durablesys.RenameExchange
		}
		if err := rename(int(self.directory.File().Fd()), pending.Temporary, int(self.directory.File().Fd()), self.spec.Name); err != nil {
			return observation(err, true)
		}
		if err := self.boundary("snapshot-renamed"); err != nil {
			return err
		}
		if hooks.AfterRename != nil {
			if err := hooks.AfterRename(); err != nil {
				return err
			}
		}
	}
	if _, err := self.readMember(self.spec.Name, pending.Next); err != nil {
		return err
	}
	_, temporaryPresent, err := self.stat(pending.Temporary, false)
	if err != nil {
		return err
	}
	if temporaryPresent {
		if !self.checkpoint.Committed.Present {
			return identity(errors.New("unexpected snapshot temporary after first publication"))
		}
		if _, err := self.readMember(pending.Temporary, self.checkpoint.Committed); err != nil {
			return err
		}
	}
	if err := self.syncMember(self.spec.Name, pending.Next); err != nil {
		return err
	}
	if err := self.directory.File().Sync(); err != nil {
		return err
	}
	if err := self.boundary("snapshot-directory-synced"); err != nil {
		return err
	}
	if hooks.AfterDirectorySync != nil {
		if err := hooks.AfterDirectorySync(self.directory.File()); err != nil {
			return err
		}
	}
	if err := self.admit(true); err != nil {
		return err
	}
	if temporaryPresent {
		// Only the fully checked exchanged predecessor is removed. Unknown or
		// unfinished next bytes are never cleanup candidates.
		if _, err := self.readMember(pending.Temporary, self.checkpoint.Committed); err != nil {
			return err
		}
		if err := unix.Unlinkat(int(self.directory.File().Fd()), pending.Temporary, 0); err != nil {
			return observation(err, true)
		}
		if err := self.boundary("predecessor-removed"); err != nil {
			return err
		}
		if err := self.directory.File().Sync(); err != nil {
			return err
		}
	}
	if _, err := self.readMember(self.spec.Name, pending.Next); err != nil {
		return err
	}
	committed := self.checkpoint
	committed.Committed, committed.Pending = pending.Next, nil
	if err := self.writeCheckpoint(committed, "committed"); err != nil {
		return err
	}
	delete(self.knownKVs, pending.Temporary)
	return self.check(false)
}

// A compact diagnostic keeps configured capacity visible without implying
// automatic archive/rotation or proving an operator's future storage budget.
func (self *Owner) Capacity() (used, maximum int64, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err := self.check(false); err != nil {
		return 0, self.spec.MaximumBytes, err
	}
	return self.checkpoint.Committed.Size, self.spec.MaximumBytes, nil
}
