// Fixture commits inherit the caller's existing global Git identity. The guard
// refuses an override before Git can write configuration or create a commit.
package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Resolve the real Git before installing a test-local interception boundary.
// Read-only identity queries use that executable and never set configuration.
func guardGitFixtureIdentity(t *testing.T) func(string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	read := func(root string, args ...string) string {
		t.Helper()
		command := exec.Command(git, append([]string{"-C", root}, args...)...)
		raw, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("read Git fixture identity: %v: %s", err, raw)
		}
		return strings.TrimSuffix(string(raw), "\n")
	}
	queryRoot := t.TempDir()
	name := read(queryRoot, "config", "--global", "--get", "user.name")
	email := read(queryRoot, "config", "--global", "--get", "user.email")
	if name == "" || email == "" {
		t.Fatal("fixture requires the existing global Git identity")
	}
	directory := t.TempDir()
	const wrapper = `#!/bin/sh
if [ "${GIT_AUTHOR_NAME+x}${GIT_AUTHOR_EMAIL+x}${GIT_COMMITTER_NAME+x}${GIT_COMMITTER_EMAIL+x}" != "" ]; then
  echo 'fixture identity override refused before Git execution' >&2
  exit 97
fi
for arg in "$@"; do
  case "$arg" in
    user.name|user.email|user.name=*|user.email=*|--author|--author=*|--config-env=user.name=*|--config-env=user.email=*)
      echo 'fixture identity override refused before Git execution' >&2
      exit 97
      ;;
  esac
done
exec "$SN_FIXTURE_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(directory, "git"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SN_FIXTURE_REAL_GIT", git)
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func(root string) {
		t.Helper()
		got := strings.Split(read(root, "show", "-s", "--format=%an%x00%ae%x00%cn%x00%ce", "HEAD"), "\x00")
		if !reflect.DeepEqual(got, []string{name, email, name, email}) {
			t.Fatal("actual fixture author or committer differs from existing global Git identity")
		}
		command := exec.Command(git, "-C", root, "config", "--local", "--get-regexp", `^user\.(name|email)$`)
		raw, err := command.CombinedOutput()
		var status *exec.ExitError
		if !errors.As(err, &status) || status.ExitCode() != 1 || len(raw) != 0 {
			t.Fatalf("fixture has local author identity configuration: %v: %s", err, raw)
		}
	}
}

// Every repository made by the existing workspace and testnet fixtures keeps
// the caller's identity without writing fixture-local author settings.
func TestReleaseGitFixturesUseGlobalIdentity(t *testing.T) {
	requireIdentity := guardGitFixtureIdentity(t)
	workspace, _ := releaseObservationWorkspaceTest(t)
	repositories := 0
	if err := filepath.WalkDir(workspace, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == ".git" {
			requireIdentity(filepath.Dir(path))
			repositories++
			return filepath.SkipDir
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if repositories == 0 {
		t.Fatal("existing workspace fixture produced no Git repository")
	}
	requireIdentity(releaseLockCheckoutVault(t))
	global, globalSet := os.LookupEnv("GIT_CONFIG_GLOBAL")
	configCount := 0
	if value := os.Getenv("GIT_CONFIG_COUNT"); value != "" {
		var err error
		configCount, err = strconv.Atoi(value)
		if err != nil || configCount < 0 {
			t.Fatal("invalid inherited Git configuration count")
		}
	}
	suffix := strconv.Itoa(configCount)
	t.Setenv("GIT_CONFIG_KEY_"+suffix, "fixture.identitycheck")
	t.Setenv("GIT_CONFIG_VALUE_"+suffix, "retained")
	t.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(configCount+1))
	freezeWorkspace := releaseSourceFreezeFixture(t)
	if value, present := os.LookupEnv("GIT_CONFIG_GLOBAL"); value != global || present != globalSet {
		t.Fatal("source freeze fixture replaced the existing global Git configuration")
	}
	root := filepath.Join(freezeWorkspace, "sn")
	if value := strings.TrimSpace(string(testGitOutput(t, root, "config", "--get", "fixture.identitycheck"))); value != "retained" {
		t.Fatal("source freeze fixture discarded inherited Git configuration")
	}
	before := string(testGitOutput(t, root, "rev-parse", "HEAD"))
	runTestGit(t, root, "commit", "--allow-empty", "-qm", "synthetic fixture branch advance")
	if after := string(testGitOutput(t, root, "rev-parse", "HEAD")); after == before {
		t.Fatal("post-fixture branch advance did not create an actual commit")
	}
	requireIdentity(root)
}
