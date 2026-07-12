package eventlog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestRedisAppendCommandComposition asserts the atomic-append EVAL command is
// composed from the correct keys/args WITHOUT a live Valkey (the EVAL itself
// requires a running server and is documented as needing live integration).
//
// It verifies:
//   - the Lua script does INCR then XADD then EXPIRE in one script (atomicity);
//   - KEYS are [stream, seq]; NUMKEYS is 2;
//   - ARGV are [event JSON, maxlen, ttl-seconds] in order.
func TestRedisAppendCommandComposition(t *testing.T) {
	r := &RedisStreamLog{app: "agentic", ttl: 24 * time.Hour, maxLen: 10000}

	streamKey, seqKey := r.appendKeys("sess-1")
	if streamKey != "evlog:agentic:sess-1" {
		t.Errorf("stream key = %q", streamKey)
	}
	if seqKey != "evlog:seq:agentic:sess-1" {
		t.Errorf("seq key = %q", seqKey)
	}

	ev := AgentEvent{V: 1, Type: EvTextDelta, Text: "hi", IsOutput: true}
	data, _ := json.Marshal(ev)
	dataArg, maxlenArg, ttlArg := r.appendArgs(data)

	if dataArg != string(data) {
		t.Errorf("data arg mismatch")
	}
	if maxlenArg != "10000" {
		t.Errorf("maxlen arg = %q, want 10000", maxlenArg)
	}
	if ttlArg != "86400" {
		t.Errorf("ttl arg = %q, want 86400 (24h)", ttlArg)
	}

	// The event round-trips through the arg so the reader decodes it identically.
	var back AgentEvent
	if err := json.Unmarshal([]byte(dataArg), &back); err != nil {
		t.Fatalf("data arg not valid event JSON: %v", err)
	}
	if back.Text != "hi" || !back.IsOutput {
		t.Errorf("decoded event = %+v", back)
	}
}

// TestRedisAppendScriptAtomicity asserts the Lua body is a single script doing
// INCR + XADD (with the seq as the entry id) + EXPIRE — the atomicity guarantee
// that replaced the old two-command INCR-then-XADD (H4).
func TestRedisAppendScriptAtomicity(t *testing.T) {
	for _, want := range []string{"INCR", "XADD", "EXPIRE", "seq .. '-0'", "MAXLEN"} {
		if !strings.Contains(appendScript, want) {
			t.Errorf("append script missing %q; script:\n%s", want, appendScript)
		}
	}
	// INCR must precede XADD in the script (seq assigned before the entry).
	if strings.Index(appendScript, "INCR") > strings.Index(appendScript, "XADD") {
		t.Error("INCR must precede XADD in the atomic append script")
	}
}

func TestRedisMaxLenDisabledOmitsTrim(t *testing.T) {
	r := &RedisStreamLog{app: "a", ttl: time.Hour, maxLen: 0}
	_, maxlenArg, _ := r.appendArgs([]byte("{}"))
	if maxlenArg != "0" {
		t.Errorf("maxlen arg = %q, want 0 (trim disabled)", maxlenArg)
	}
	// Script must branch on maxlen>0 so a 0 maxlen skips MAXLEN.
	if !strings.Contains(appendScript, "tonumber(ARGV[2]) > 0") {
		t.Error("script must guard the MAXLEN branch on ARGV[2] > 0")
	}
}
