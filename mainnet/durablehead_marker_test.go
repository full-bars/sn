//go:build linux || darwin

package main

// Create markers are transient head metadata: the restore planner admits one
// with the head pair, never as an application member and never as a signed
// member. The 3.23-fix planner still refuses one (v33.1 item), so the sn
// allowance is pinned here.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/urnetwork/connect/durablevolume"

	"github.com/urfoundation/sn/internal/durablefixture"
	"github.com/urfoundation/sn/internal/durablehead"
	"github.com/urfoundation/sn/internal/durablepath"
)

func markerTestSpec(local bool) durablehead.Spec {
	if local {
		return durablehead.Spec{Kind: "mainnet-successor-local-members", Name: ".successor-local-members.json", MaximumBytes: 8 << 20}
	}
	return durablehead.Spec{Kind: "mainnet-successor-nonce-members", Name: ".successor-nonce-members.json", MaximumBytes: 8 << 20}
}

func TestParseCreateMarkerShape(t *testing.T) {
	spec := markerTestSpec(true)
	other := markerTestSpec(false)
	nonce := strings.Repeat("ab", 16)
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"marker", durablehead.CreateMarker(spec, nonce), true},
		{"temporary", durablehead.HeadPrefix(spec) + nonce, false},
		{"member", spec.Name, false},
		{"empty", "", false},
		{"short-nonce", durablehead.HeadPrefix(spec) + "create-" + nonce[:30], false},
		{"long-nonce", durablehead.HeadPrefix(spec) + "create-" + nonce + "00", false},
		{"non-hex-nonce", durablehead.HeadPrefix(spec) + "create-" + strings.Repeat("zz", 16), false},
		{"other-spec-marker", durablehead.CreateMarker(other, nonce), false},
		{"marker-of-temporary", durablehead.HeadPrefix(spec) + "create-" + spec.Name, false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			temporary, ok := durablehead.ParseCreateMarker(spec, item.path)
			if ok != item.want {
				t.Fatalf("ParseCreateMarker(%q) = %t, want %t", item.path, ok, item.want)
			}
			if ok && temporary != durablehead.HeadPrefix(spec)+nonce {
				t.Fatalf("temporary = %q", temporary)
			}
		})
	}
}

func markerInventoryStat(t *testing.T, path string) unix.Stat_t {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		t.Fatal(err)
	}
	return stat
}

func markerPhysical(stat unix.Stat_t) *durablevolume.PhysicalRoot {
	return &durablevolume.PhysicalRoot{
		Device: durablevolume.Device{Major: unix.Major(stat.Dev), Minor: unix.Minor(stat.Dev)},
		Inode:  stat.Ino,
	}
}

func markerSha(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func markerCheckpointRaw(t *testing.T, path, name string) []byte {
	t.Helper()
	raw := make([]byte, 4096)
	n, err := unix.Getxattr(path, name, raw)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), raw[:n]...)
}

// A marker in the source inventory is restored with the head pair: the plan
// admits it, keeps it as a zero-byte file, and never treats it as a member.
// Without the marker allowance the same plan is refused as an unreviewed
// owner member, which is still the 3.23-fix behaviour.
func TestPlanRestoreAdmitsCreateMarker(t *testing.T) {
	spec := durablehead.Spec{Kind: "synthetic-restore-marker", Name: "history.json", MaximumBytes: 4096, LockName: "history.lock", AuxiliaryNames: []string{"history.lock"}}
	root := filepath.Join(t.TempDir(), "owner")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	fixture := durablefixture.New(t, t.Context(), root)
	durablefixture.ProvisionSnapshot(t, root, spec.Kind, spec.Name, spec.MaximumBytes, spec.LockName, map[string][]byte{spec.LockName: []byte("prepared\n")})
	directory, err := durablepath.Open(fixture.Context, root, durablevolume.ReadWrite, false)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(root, spec.LockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := durablehead.Open(fixture.Context, directory, lock, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Publish([]byte("committed"), nil); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(owner.Close(), lock.Close(), directory.Close()); err != nil {
		t.Fatal(err)
	}

	marker := durablehead.CreateMarker(spec, strings.Repeat("cd", 16))
	if err := os.WriteFile(filepath.Join(root, marker), nil, 0600); err != nil {
		t.Fatal(err)
	}
	memberRaw, err := os.ReadFile(filepath.Join(root, spec.Name))
	if err != nil {
		t.Fatal(err)
	}
	lockRaw, err := os.ReadFile(filepath.Join(root, spec.LockName))
	if err != nil {
		t.Fatal(err)
	}
	rootStat := markerInventoryStat(t, root)
	memberStat := markerInventoryStat(t, filepath.Join(root, spec.Name))
	lockStat := markerInventoryStat(t, filepath.Join(root, spec.LockName))
	markerStat := markerInventoryStat(t, filepath.Join(root, marker))
	checkpointRaw := markerCheckpointRaw(t, filepath.Join(root, spec.LockName), durablehead.Attribute(spec.Kind, spec.Name))
	report := durablevolume.Inventory{
		Schema:       durablevolume.PhysicalInventorySchema,
		PhysicalRoot: *markerPhysical(rootStat),
		Entries: []durablevolume.InventoryEntry{
			{Path: "", Kind: "directory", Mode: 0700, Physical: markerPhysical(rootStat)},
			{Path: spec.Name, Kind: "file", Mode: 0600, Size: uint64(len(memberRaw)), Sha256: markerSha(memberRaw), Physical: markerPhysical(memberStat)},
			{Path: spec.LockName, Kind: "file", Mode: 0600, Size: uint64(len(lockRaw)), Sha256: markerSha(lockRaw), Physical: markerPhysical(lockStat),
				OwnerAttributes: []durablevolume.InventoryAttribute{{Name: durablehead.Attribute(spec.Kind, spec.Name), Value: checkpointRaw, Sha256: markerSha(checkpointRaw)}}},
			{Path: marker, Kind: "file", Mode: 0600, Size: 0, Sha256: markerSha(nil), Physical: markerPhysical(markerStat)},
		},
	}
	plan, err := durablehead.PlanRestore(fixture.Context, "restore-marker", durablevolume.PreparationOwner{Purpose: "restore", Kind: spec.Kind, RelativePath: "."}, spec, json.RawMessage(`{"profile":"synthetic-restore-marker"}`), report)
	if err != nil {
		t.Fatalf("plan restore with a create marker: %v", err)
	}
	found := false
	for _, file := range plan.Files {
		if file.Path == marker {
			found = true
			if file.Kind != "file" || file.Mode != 0600 || file.Bytes != 0 {
				t.Fatalf("marker file = %+v", file)
			}
		}
	}
	if !found {
		t.Fatalf("plan omitted the create marker: %+v", plan.Files)
	}
}
