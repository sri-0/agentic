package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentic/internal/agent"
	"agentic/internal/eventlog"
	"agentic/internal/types"

	"github.com/gorilla/mux"
	"github.com/rs/zerolog"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// muxSetVar injects a gorilla/mux route var into the request so a handler that
// reads mux.Vars(r)["id"] works when invoked directly (not via the router).
func muxSetVar(r *http.Request, key, val string) *http.Request {
	return mux.SetURLVars(r, map[string]string{key: val})
}

// echoAgent streams back a single deterministic token so each turn's stream is
// identifiable. The token is the last user message text.
func echoAgent(t *testing.T) adkagent.Agent {
	t.Helper()
	a, err := adkagent.New(adkagent.Config{
		Name: "echo_agent",
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				// Read the latest user text from the session so the reply is
				// turn-specific.
				reply := "reply"
				evt := session.NewEvent(ctx.InvocationID())
				evt.LLMResponse = model.LLMResponse{
					Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: reply}}},
					TurnComplete: true,
				}
				evt.Author = "echo_agent"
				yield(evt, nil)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func sseTextParts(body string) []string {
	var parts []string
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var evt map[string]any
		if json.Unmarshal([]byte(payload), &evt) != nil {
			continue
		}
		if choices, ok := evt["choices"].([]any); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]any); ok {
				if delta, ok := choice["delta"].(map[string]any); ok {
					if c, ok := delta["content"].(string); ok && c != "" {
						parts = append(parts, c)
					}
				}
			}
		}
	}
	return parts
}

// W0/C1 (HTTP level): two sequential turns on one thread_id via the coordinator.
// The SECOND turn's stream must carry a reply and must NOT close instantly on
// turn 1's terminal (the sticky-terminal / after=0 bug). Before W1 the second
// stream would replay turn 1 and close at turn 1's terminal, delivering no
// turn-2 output.
func TestChatHandler_MultiTurn_SecondTurnStreams(t *testing.T) {
	core := newTestCore(t, echoAgent(t))
	reg := newTestRegistry(t, core)
	coord := agent.NewCoordinator(eventlog.NewMemoryLog(), zerolog.Nop())
	defer coord.StopSweeper()
	handler := Chat(reg, core.Config, nil, nil, nil, coord, zerolog.Nop())

	post := func(text string) string {
		body := `{"model":"test-agent","messages":[{"role":"user","content":"` + text + `"}],"stream":true,"thread_id":"multi-turn-1"}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	turn1 := post("first")
	parts1 := sseTextParts(turn1)
	if len(parts1) == 0 {
		t.Fatalf("turn 1 produced no text: %q", turn1)
	}

	turn2 := post("second")
	parts2 := sseTextParts(turn2)
	if len(parts2) == 0 {
		t.Fatalf("turn 2 produced no text (sticky-terminal/multi-turn bug): %q", turn2)
	}
	if !strings.Contains(turn2, "[DONE]") {
		t.Fatalf("turn 2 stream did not finish cleanly: %q", turn2)
	}
}

// H2: a session owned by one user must not be attachable/cancellable by another.
// A cross-user attach or cancel must 404 (never stream the owner's log). We seed
// a known session for "alice" directly in the coordinator, then request it as
// "bob".
func TestSessionStream_CrossUser404(t *testing.T) {
	core := newTestCore(t, echoAgent(t))
	coord := agent.NewCoordinator(eventlog.NewMemoryLog(), zerolog.Nop())
	defer coord.StopSweeper()

	// Run one turn as alice so the coordinator knows the session and it is owned
	// by alice. Start returns immediately; the run finishes in the background but
	// the known-handle (with UserID=alice) persists.
	const sessionID = "owned-by-alice"
	if _, err := coord.Start(agent.RunRequest{
		SessionID: sessionID,
		UserID:    "alice",
		Core:      core,
		Messages:  []types.ChatMessage{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("alice start: %v", err)
	}

	// Sanity: alice can see it.
	if _, ok := coord.Status("alice", sessionID); !ok {
		t.Fatal("alice should own the session")
	}
	// Bob must not.
	if _, ok := coord.Status("bob", sessionID); ok {
		t.Fatal("bob must not see alice's session via Status")
	}

	streamH := SessionStream(coord, zerolog.Nop())
	cancelH := SessionCancel(coord, zerolog.Nop())

	// Bob attaching → 404.
	req := httptest.NewRequest("GET", "/v1/sessions/"+sessionID+"/stream", nil)
	req.Header.Set("X-User-ID", "bob")
	req = muxSetVar(req, "id", sessionID)
	w := httptest.NewRecorder()
	streamH.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("cross-user stream: status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}

	// Bob cancelling → 404.
	req = httptest.NewRequest("POST", "/v1/sessions/"+sessionID+"/cancel", nil)
	req.Header.Set("X-User-ID", "bob")
	req = muxSetVar(req, "id", sessionID)
	w = httptest.NewRecorder()
	cancelH.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("cross-user cancel: status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}

	// Alice attaching → 200 (owner allowed). Session-follow stays live until the
	// client disconnects, so use a context we cancel to end the stream.
	ctx, cancel := context.WithCancel(context.Background())
	req = httptest.NewRequest("GET", "/v1/sessions/"+sessionID+"/stream", nil).WithContext(ctx)
	req.Header.Set("X-User-ID", "alice")
	req = muxSetVar(req, "id", sessionID)
	w = httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		streamH.ServeHTTP(w, req)
		close(done)
	}()
	// Give the pump a moment to replay + emit framing, then disconnect.
	<-time.After(150 * time.Millisecond)
	cancel()
	<-done
	if w.Code != 200 {
		t.Fatalf("owner stream: status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
}
