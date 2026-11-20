package miner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/miner/bandwidth"
)

func TestProxySecretCatchAllFlow(t *testing.T) {
	dir := withTempHome(t)
	secrets := []string{"canary-user-7731", "canary-pass-9042", "canary-token-5518"}
	line := "127.0.0.1:1080:" + secrets[0] + ":" + secrets[1]
	runAdd := func(args ...string) string {
		t.Helper()
		encoded, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestProxyAddCredentialsCLIChild$")
		cmd.Env = append(os.Environ(), "SN_PROXY_ADD_CHILD=1", "SN_PROXY_ADD_ARGS="+string(encoded), "HOME="+dir, "URNETWORK_STATE_DIR="+dir, "WARP_VERSION=synthetic")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("proxy add failed: %v", err)
		}
		return string(out)
	}
	output := runAdd("proxy", "add", line, "-f")
	input := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(input, []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output += runAdd("proxy", "add", "--proxy_file="+input, "-f")
	config := readProxyConfig()
	if len(config.Servers) == 0 {
		t.Fatal("proxy was not added")
	}
	args, err := json.Marshal([]string{"provide", "--api_url=http://127.0.0.1:1", "--connect_url=ws://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProxyAddCredentialsCLIChild$")
	child.Env = append(os.Environ(), "SN_PROXY_ADD_CHILD=1", "SN_PROXY_ADD_ARGS="+string(args), "HOME="+dir, "URNETWORK_STATE_DIR="+dir, "WARP_VERSION=synthetic")
	startup, startErr := child.CombinedOutput()
	var exitErr *exec.ExitError
	if startErr != nil && ctx.Err() == nil && !errors.As(startErr, &exitErr) {
		t.Fatalf("start provider child: %v", startErr)
	}
	output += string(startup)
	resetProxyAuditStatus(t)
	resetProxyEarnBandwidthForTest()
	t.Cleanup(resetProxyEarnBandwidthForTest)
	address := "socks5://" + secrets[0] + ":" + secrets[1] + "@example.com:1080?token=" + secrets[2]
	publishProxyEarnBandwidth(address, &bandwidth.ProxyBandwidth{})
	state := &ProxyState{Proxies: map[string]ProxyEntry{address: {ID: 77, Health: "dead"}}}
	now := time.Now()
	report := buildProxyHealthReport(state, now)
	writeProxyHealthState(dir, report, now)
	writeProxyTrafficState(dir, report, now)
	report.NewlyDead = []proxyEvent{{Index: 77, Address: proxyKeyDisplay(address)}}
	writeProxyHealthEvents(dir, report, now)
	h := newGovHarness("a:1")
	h.g.st.parks[address] = &proxyAuditParkRecord{until: now.Add(time.Minute)}
	h.g.publish(now, true, proxyAuditResult{})
	for _, command := range []string{"status", "snapshot", "audit"} {
		data, err := json.Marshal(handleControlRequest(newControlState(), controlRequest{Cmd: command}))
		if err != nil {
			t.Fatal(err)
		}
		output += string(data)
	}
	for _, secret := range secrets {
		if strings.Contains(output, secret) {
			t.Fatal("output contains canary")
		}
	}
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if path == filepath.Join(dir, "proxy") {
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Error("proxy config is not private")
			}
			if !strings.Contains(string(data), secrets[1]) {
				t.Error("private proxy config is missing credentials")
			}
			return nil
		}
		for _, secret := range secrets {
			if strings.Contains(string(data), secret) {
				t.Errorf("canary in %s", filepath.Base(path))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
