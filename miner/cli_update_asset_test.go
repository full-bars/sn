package miner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func releaseWithAssets(tag string, names ...string) *minerRelease {
	release := &minerRelease{TagName: tag}
	for _, name := range names {
		release.Assets = append(release.Assets, minerReleaseAsset{Name: name, Digest: "sha256:00"})
	}
	return release
}

// The fork publishes the miner as urnetwork-miner-<tag>-<os>-<arch>.tar.gz.
func TestMinerUpdateTakesTheForksMinerArchive(t *testing.T) {
	tag := "v2026.10.23-1079000000"
	want := fmt.Sprintf("urnetwork-miner-%s-%s-%s.tar.gz", tag, runtime.GOOS, runtime.GOARCH)
	release := releaseWithAssets(tag,
		"urnetwork-miner-"+tag+"-plan9-mips.tar.gz",
		want,
		fmt.Sprintf("urnetwork-provider-%s-%s-%s.tar.gz", tag, runtime.GOOS, runtime.GOARCH),
		"urnetwork-provider-"+tag+".tar.gz",
	)
	asset, err := release.asset()
	if err != nil {
		t.Fatal(err)
	}
	if asset.Name != want {
		t.Fatalf("took %q, want %q", asset.Name, want)
	}
}

// The relay archives hold a different program. A release with only those must
// not be installed over the miner, whatever the naming.
func TestMinerUpdateNeverTakesTheRelayArchive(t *testing.T) {
	tag := "v2026.10.23-1079000000"
	bare := strings.TrimPrefix(tag, "v")
	release := releaseWithAssets(tag,
		fmt.Sprintf("urnetwork-provider-%s-%s-%s.tar.gz", tag, runtime.GOOS, runtime.GOARCH),
		fmt.Sprintf("urnetwork-provider-%s-%s-%s.tar.gz", bare, runtime.GOOS, runtime.GOARCH),
		"urnetwork-provider-"+tag+".tar.gz",
		"urnetwork-provider-"+bare+".tar.gz",
		"urnet-tools-"+runtime.GOOS+"-"+runtime.GOARCH,
	)
	asset, err := release.asset()
	if err == nil {
		t.Fatalf("a release with only relay archives must have no miner download, took %q", asset.Name)
	}
	if !strings.Contains(err.Error(), "no miner download") || !strings.Contains(err.Error(), tag) {
		t.Fatalf("the error must name the tag and say there is no miner download, got %q", err)
	}
}

// The archive's lone member is the miner named provider; that is what the
// update installs over the running binary.
func TestMinerUpdateExtractsTheMinerFromItsArchive(t *testing.T) {
	member := "provider"
	if runtime.GOOS == "windows" {
		member += ".exe"
	}
	payload := []byte("miner-binary-bytes")
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: member, Mode: 0755, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()

	var out bytes.Buffer
	found, err := extractMinerBinary(&raw, &out)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Fatalf("extracted %q, want %q", out.Bytes(), payload)
	}
}
