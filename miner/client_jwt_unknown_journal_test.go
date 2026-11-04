package miner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

func TestJWTUnknownJournalRefusesEverySlot(t *testing.T) {
	dir := t.TempDir()
	defer withGlobalStore(t, filepath.Join(dir, legacyClientJwtStoreName))()
	setHotRestart(t, true)
	journal := `{"schema":"future-schema","slots":{"imported-slot":{"state":"imported"},"fallback-slot":{"state":"fallback"}}}`
	if err := os.WriteFile(filepath.Join(dir, legacyImportJournalName), []byte(journal), 0600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"unrelated-slot", "imported-slot", "fallback-slot"} {
		t.Run(key, func(t *testing.T) {
			calls := 0
			mint := func(context.Context) (string, connect.Id, error) {
				calls++
				return "minted", connect.NewId(), nil
			}
			_, _, _, err := provideClientJWT(context.Background(), clientJWTProvideConfig{IdentityKey: key, ClientJwtPath: filepath.Join(dir, ".provider.jwt"), Mint: mint})
			if err == nil || !strings.Contains(err.Error(), legacyImportJournalName) || !strings.Contains(err.Error(), "refuses to authenticate") {
				t.Fatalf("error = %v, want journal name and authentication refusal", err)
			}
			if calls != 0 {
				t.Fatalf("mint calls = %d, want none", calls)
			}
		})
	}
}
