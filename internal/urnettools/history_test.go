package urnettools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestParseHistoryArgsDefaults: bare `history` returns limit=50, no cursor.
func TestParseHistoryArgsDefaults(t *testing.T) {
	limit, cursor, _, err := parseHistoryArgs(nil)
	if err != nil {
		t.Fatalf("parseHistoryArgs(nil) = %v", err)
	}
	if limit != 50 {
		t.Errorf("limit = %d, want 50", limit)
	}
	if cursor != "" {
		t.Errorf("cursor = %q, want empty", cursor)
	}
}

// TestParseHistoryArgsPositionalLimit: `history 30` parses the limit.
func TestParseHistoryArgsPositionalLimit(t *testing.T) {
	limit, cursor, _, err := parseHistoryArgs([]string{"30"})
	if err != nil {
		t.Fatalf("parseHistoryArgs([30]) = %v", err)
	}
	if limit != 30 {
		t.Errorf("limit = %d, want 30", limit)
	}
	if cursor != "" {
		t.Errorf("cursor = %q, want empty", cursor)
	}
}

// TestParseHistoryArgsLimitCapped: limit > 100 is capped to 100.
func TestParseHistoryArgsLimitCapped(t *testing.T) {
	limit, _, _, err := parseHistoryArgs([]string{"500"})
	if err != nil {
		t.Fatalf("parseHistoryArgs([500]) = %v", err)
	}
	if limit != 100 {
		t.Errorf("limit = %d, want 100 (capped)", limit)
	}
}

// TestParseHistoryArgsInvalidLimit: non-numeric limit must error.
func TestParseHistoryArgsInvalidLimit(t *testing.T) {
	_, _, _, err := parseHistoryArgs([]string{"abc"})
	if err == nil {
		t.Fatal("expected error for non-numeric limit")
	}
	if !strings.Contains(err.Error(), "positive integer") {
		t.Errorf("error = %v, want positive-integer message", err)
	}
}

// TestParseHistoryArgsNegativeLimit: negative limit must error.
func TestParseHistoryArgsNegativeLimit(t *testing.T) {
	_, _, _, err := parseHistoryArgs([]string{"-5"})
	if err == nil {
		t.Fatal("expected error for negative limit")
	}
	if !strings.Contains(err.Error(), "positive integer") {
		t.Errorf("error = %v, want positive-integer message", err)
	}
}

// TestParseHistoryArgsWithTarget: `history --unit X` must NOT fail.
// This is the primary bug: Atoi ran before parseTargetFlags, so the flag
// was treated as the limit.
func TestParseHistoryArgsWithTarget(t *testing.T) {
	limit, cursor, tgt, err := parseHistoryArgs([]string{"--unit", "foo.service"})
	if err != nil {
		t.Fatalf("parseHistoryArgs([--unit foo.service]) = %v", err)
	}
	if limit != 50 {
		t.Errorf("limit = %d, want 50 (default)", limit)
	}
	if cursor != "" {
		t.Errorf("cursor = %q, want empty", cursor)
	}
	if tgt.Unit != "foo.service" {
		t.Errorf("Unit = %q, want foo.service", tgt.Unit)
	}
}

// TestParseHistoryArgsLimitBeforeTarget: `history 20 --unit X`
func TestParseHistoryArgsLimitBeforeTarget(t *testing.T) {
	limit, _, tgt, err := parseHistoryArgs([]string{"20", "--unit", "bar.service"})
	if err != nil {
		t.Fatalf("parseHistoryArgs([20 --unit bar.service]) = %v", err)
	}
	if limit != 20 {
		t.Errorf("limit = %d, want 20", limit)
	}
	if tgt.Unit != "bar.service" {
		t.Errorf("Unit = %q, want bar.service", tgt.Unit)
	}
}

// TestParseHistoryArgsCursorSpace: `history 50 --cursor abc123`
func TestParseHistoryArgsCursorSpace(t *testing.T) {
	limit, cursor, _, err := parseHistoryArgs([]string{"50", "--cursor", "abc123"})
	if err != nil {
		t.Fatalf("parseHistoryArgs([50 --cursor abc123]) = %v", err)
	}
	if limit != 50 {
		t.Errorf("limit = %d, want 50", limit)
	}
	if cursor != "abc123" {
		t.Errorf("cursor = %q, want abc123", cursor)
	}
}

// TestParseHistoryArgsCursorEquals: `history --cursor=xyz`
func TestParseHistoryArgsCursorEquals(t *testing.T) {
	limit, cursor, _, err := parseHistoryArgs([]string{"--cursor=xyz"})
	if err != nil {
		t.Fatalf("parseHistoryArgs([--cursor=xyz]) = %v", err)
	}
	if limit != 50 {
		t.Errorf("limit = %d, want 50", limit)
	}
	if cursor != "xyz" {
		t.Errorf("cursor = %q, want xyz", cursor)
	}
}

// TestParseHistoryArgsCursorNoValue: `history --cursor` must error.
func TestParseHistoryArgsCursorNoValue(t *testing.T) {
	_, _, _, err := parseHistoryArgs([]string{"--cursor"})
	if err == nil {
		t.Fatal("expected error for --cursor without value")
	}
	if !strings.Contains(err.Error(), "requires a value") {
		t.Errorf("error = %v, want requires-a-value message", err)
	}
}

// TestParseHistoryArgsCursorEqualsEmpty: `history --cursor=` must error.
func TestParseHistoryArgsCursorEqualsEmpty(t *testing.T) {
	_, _, _, err := parseHistoryArgs([]string{"--cursor="})
	if err == nil {
		t.Fatal("expected error for --cursor= with empty value")
	}
	if !strings.Contains(err.Error(), "requires a value") {
		t.Errorf("error = %v, want requires-a-value message", err)
	}
}

// TestParseHistoryArgsCursorBeforeTarget: `history 10 --cursor C --unit X`
func TestParseHistoryArgsCursorBeforeTarget(t *testing.T) {
	limit, cursor, tgt, err := parseHistoryArgs([]string{"10", "--cursor", "tok42", "--unit", "svc"})
	if err != nil {
		t.Fatalf("parseHistoryArgs([10 --cursor tok42 --unit svc]) = %v", err)
	}
	if limit != 10 {
		t.Errorf("limit = %d, want 10", limit)
	}
	if cursor != "tok42" {
		t.Errorf("cursor = %q, want tok42", cursor)
	}
	if tgt.Unit != "svc" {
		t.Errorf("Unit = %q, want svc", tgt.Unit)
	}
}

// TestParseHistoryArgsTargetOnly: `history --user alice`
func TestParseHistoryArgsTargetOnly(t *testing.T) {
	limit, cursor, tgt, err := parseHistoryArgs([]string{"--user", "alice"})
	if err != nil {
		t.Fatalf("parseHistoryArgs([--user alice]) = %v", err)
	}
	if limit != 50 {
		t.Errorf("limit = %d, want 50", limit)
	}
	if cursor != "" {
		t.Errorf("cursor = %q, want empty", cursor)
	}
	if tgt.User != "alice" {
		t.Errorf("User = %q, want alice", tgt.User)
	}
}

// TestParseHistoryArgsCursorOnly: `history --cursor abc` with no limit
func TestParseHistoryArgsCursorOnly(t *testing.T) {
	limit, cursor, _, err := parseHistoryArgs([]string{"--cursor", "abc"})
	if err != nil {
		t.Fatalf("parseHistoryArgs([--cursor abc]) = %v", err)
	}
	if limit != 50 {
		t.Errorf("limit = %d, want 50", limit)
	}
	if cursor != "abc" {
		t.Errorf("cursor = %q, want abc", cursor)
	}
}

// TestAuditEntryWireFormatRoundTrip verifies that the client's AuditEntry
// can deserialize the JSON the provider's CommandAudit actually produces
// over the wire (Timestamp is time.Time -> RFC3339 string, not int64).
func TestAuditEntryWireFormatRoundTrip(t *testing.T) {
	// Simulate what the provider sends: a JSON object with an RFC3339 timestamp string.
	wireJSON := `{"timestamp":"2026-09-12T10:30:00Z","cmd":"set","key":"turbo","value":"v8","ok":true}`

	var entry AuditEntry
	if err := json.Unmarshal([]byte(wireJSON), &entry); err != nil {
		t.Fatalf("failed to unmarshal provider wire JSON into AuditEntry: %v", err)
	}
	if entry.Cmd != "set" {
		t.Errorf("Cmd = %q, want set", entry.Cmd)
	}
	if entry.Key != "turbo" {
		t.Errorf("Key = %q, want turbo", entry.Key)
	}
	if entry.Value != "v8" {
		t.Errorf("Value = %q, want v8", entry.Value)
	}
	if !entry.OK {
		t.Error("OK = false, want true")
	}
	// Parse the timestamp and verify it round-trips correctly.
	ts, err := time.Parse(time.RFC3339, entry.Timestamp)
	if err != nil {
		t.Fatalf("failed to parse Timestamp %q as RFC3339: %v", entry.Timestamp, err)
	}
	want := time.Date(2026, 9, 12, 10, 30, 0, 0, time.UTC)
	if !ts.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", ts, want)
	}
}

// TestAuditEntryWireFormatFullResponse verifies the full controlResponse
// deserialization including entries with timestamps.
func TestAuditEntryWireFormatFullResponse(t *testing.T) {
	wireJSON := `{
		"ok": true,
		"entries": [
			{"timestamp":"2026-09-12T10:30:00Z","cmd":"set","key":"turbo","value":"v8","ok":true},
			{"timestamp":"2026-09-12T10:31:00Z","cmd":"clear","key":"turbo","error":"not found","ok":false}
		],
		"next_cursor": "2026-09-12T10:31:00Z"
	}`

	var resp controlResponse
	if err := json.Unmarshal([]byte(wireJSON), &resp); err != nil {
		t.Fatalf("failed to unmarshal full response: %v", err)
	}
	if !resp.OK {
		t.Error("OK = false, want true")
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("Entries len = %d, want 2", len(resp.Entries))
	}
	if resp.Entries[0].Cmd != "set" {
		t.Errorf("Entries[0].Cmd = %q, want set", resp.Entries[0].Cmd)
	}
	if !resp.Entries[0].OK {
		t.Error("Entries[0].OK = false, want true")
	}
	if resp.Entries[1].Cmd != "clear" {
		t.Errorf("Entries[1].Cmd = %q, want clear", resp.Entries[1].Cmd)
	}
	if resp.Entries[1].OK {
		t.Error("Entries[1].OK = true, want false")
	}
	if resp.Entries[1].Error != "not found" {
		t.Errorf("Entries[1].Error = %q, want not found", resp.Entries[1].Error)
	}
	if resp.NextCursor != "2026-09-12T10:31:00Z" {
		t.Errorf("NextCursor = %q, want 2026-09-12T10:31:00Z", resp.NextCursor)
	}
}
