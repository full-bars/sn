//go:build !windows

package urnettools

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadStateFileNoFollowRejectsSymlink: readStateFileNoFollow must refuse
// to follow a symlink planted at the state-file path — a TOCTOU-free check
// (O_NOFOLLOW open on unix) means the link cannot be swapped between check
// and read.
func TestReadStateFileNoFollowRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "jwt")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if _, err := readStateFileNoFollow(dir, "jwt"); err == nil {
		t.Fatal("readStateFileNoFollow followed a symlink; must refuse")
	}
	if b, _ := os.ReadFile(victim); string(b) != "secret" {
		t.Fatal("victim file was modified")
	}
}

// TestContainerReadFileDecodesCPTar: containerReadFile must decode the
// `docker cp -` tar stream and return the extracted file content, not raw
// tar bytes.
func TestContainerReadFileDecodesCPTar(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := []byte("jwt-content\n")
	if err := tw.WriteHeader(&tar.Header{Name: "jwt", Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	tarPath := filepath.Join(dir, "cp-output.tar")
	if err := os.WriteFile(tarPath, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	shim := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"cp\" ]; then cat \"$TEST_TAR\"; exit 0; fi\n" +
		"echo \"$*\" >> \"$DOCKER_SHIM_LOG\"\nexit 0\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setDockerTestBin(shim)
	t.Cleanup(func() { setDockerTestBin("") })
	t.Setenv("TEST_TAR", tarPath)

	out, err := containerReadFile(dockerContainer{
		ID:    "abc123",
		Name:  "urnet-test",
		Image: "urnetwork:latest",
		State: "running",
	}, "/root/.urnetwork/jwt")
	if err != nil {
		t.Fatalf("containerReadFile: %v", err)
	}
	if strings.TrimSpace(out) != "jwt-content" {
		t.Fatalf("containerReadFile returned tar bytes instead of raw file: %q", out)
	}
}
