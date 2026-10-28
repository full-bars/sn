package miner

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/urnetwork/connect"
)

// A nil candidate represents the direct identity. It is not one of the proxies
// capped by h3=N, whether the provider has no proxies or a mixed pool.
func TestControlSocketH3DirectAndProxyCandidates(t *testing.T) {
	proxies := testH3Proxies(4)
	for _, tc := range []struct {
		name       string
		candidates []*connect.ProxySettings
		wantRanked int
	}{
		{"direct-only", []*connect.ProxySettings{nil}, 0},
		{"direct-and-proxies", []*connect.ProxySettings{nil, proxies[0], proxies[1], proxies[2], proxies[3]}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := newControlState()
			path, cleanup := testControlSocket(t, state)
			defer cleanup()
			oldSetting := h3AppliedSetting.Load()
			oldRanking := h3RankingValue.Load()
			h3CandidatesMutex.Lock()
			oldCandidates := h3Candidates
			h3CandidatesMutex.Unlock()
			t.Cleanup(func() {
				h3AppliedSetting.Store(oldSetting)
				h3RankingValue.Store(oldRanking)
				h3CandidatesMutex.Lock()
				h3Candidates = oldCandidates
				h3CandidatesMutex.Unlock()
			})
			h3RankingValue.Store(nil)
			direct := h3Setting{Mode: h3ModeDirect}
			h3AppliedSetting.Store(&direct)
			applyH3CandidateSet(tc.candidates, direct)

			raw := `{"cmd":"set","key":"h3","value":"3"}`
			reply := controlExchange(t, path, raw)
			t.Logf("request=%s reply=%s", raw, reply)
			var resp controlResponse
			if err := json.Unmarshal([]byte(reply), &resp); err != nil {
				t.Fatal(err)
			}
			if !resp.OK || resp.NeedsRestart || resp.Effect != "live" {
				t.Fatalf("unexpected reply: %+v", resp)
			}
			got := controlRequestJSON(t, path, controlRequest{Cmd: "get", Key: "h3"})
			if !got.OK || !got.Found || got.Value != "3" {
				t.Fatalf("readback: %+v", got)
			}
			statePath, err := controlStatePath()
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			var envelope controlStateEnvelope
			if err := json.Unmarshal(data, &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Values["h3"] != "3" {
				t.Fatalf("persisted: %s", data)
			}
			ranking := h3RankingValue.Load()
			if ranking == nil || ranking.cap != 3 || len(ranking.keys) != tc.wantRanked {
				t.Fatalf("ranking = %+v, want %d proxies with cap 3", ranking, tc.wantRanked)
			}
			for key := range ranking.keys {
				found := false
				for _, proxy := range proxies {
					found = found || key == proxy.Key()
				}
				if !found {
					t.Fatalf("ranked non-proxy key %q", key)
				}
			}
		})
	}
}
