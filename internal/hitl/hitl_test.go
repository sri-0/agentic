package hitl

import (
	"sync"
	"testing"
)

// storeTests runs the full test suite against any Store implementation.
func storeTests(t *testing.T, store Store) {
	t.Helper()

	t.Run("GetMissing", func(t *testing.T) {
		got, err := store.Get("nonexistent")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Fatalf("expected nil for missing key, got %+v", got)
		}
	})

	t.Run("SetAndGet", func(t *testing.T) {
		p := &PendingInterrupt{
			ConfirmationCallID: "confirm_1",
			ToolCallID:         "tool_1",
			ToolName:           "write_database",
			Prompt:             "Are you sure?",
			Details:            map[string]any{"table": "users", "rows": float64(42)},
		}
		if err := store.Set("thread-1", p); err != nil {
			t.Fatalf("Set: %v", err)
		}

		got, err := store.Get("thread-1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got.ConfirmationCallID != "confirm_1" {
			t.Errorf("ConfirmationCallID = %q, want %q", got.ConfirmationCallID, "confirm_1")
		}
		if got.ToolCallID != "tool_1" {
			t.Errorf("ToolCallID = %q, want %q", got.ToolCallID, "tool_1")
		}
		if got.ToolName != "write_database" {
			t.Errorf("ToolName = %q, want %q", got.ToolName, "write_database")
		}
		if got.Prompt != "Are you sure?" {
			t.Errorf("Prompt = %q, want %q", got.Prompt, "Are you sure?")
		}
		if got.Details["table"] != "users" {
			t.Errorf("Details[table] = %v, want %q", got.Details["table"], "users")
		}
		if got.Details["rows"] != float64(42) {
			t.Errorf("Details[rows] = %v, want %v", got.Details["rows"], float64(42))
		}
	})

	t.Run("Overwrite", func(t *testing.T) {
		_ = store.Set("thread-overwrite", &PendingInterrupt{ToolName: "first"})
		_ = store.Set("thread-overwrite", &PendingInterrupt{ToolName: "second"})

		got, err := store.Get("thread-overwrite")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ToolName != "second" {
			t.Errorf("ToolName = %q, want %q after overwrite", got.ToolName, "second")
		}
	})

	t.Run("Clear", func(t *testing.T) {
		_ = store.Set("thread-clear", &PendingInterrupt{ToolName: "delete_me"})

		if err := store.Clear("thread-clear"); err != nil {
			t.Fatalf("Clear: %v", err)
		}

		got, err := store.Get("thread-clear")
		if err != nil {
			t.Fatalf("Get after Clear: %v", err)
		}
		if got != nil {
			t.Fatalf("expected nil after Clear, got %+v", got)
		}
	})

	t.Run("ClearMissing", func(t *testing.T) {
		if err := store.Clear("never-existed"); err != nil {
			t.Fatalf("Clear on missing key should not error: %v", err)
		}
	})

	t.Run("IsolatedThreads", func(t *testing.T) {
		_ = store.Set("thread-a", &PendingInterrupt{ToolName: "alpha"})
		_ = store.Set("thread-b", &PendingInterrupt{ToolName: "beta"})

		a, _ := store.Get("thread-a")
		b, _ := store.Get("thread-b")

		if a.ToolName != "alpha" {
			t.Errorf("thread-a ToolName = %q, want %q", a.ToolName, "alpha")
		}
		if b.ToolName != "beta" {
			t.Errorf("thread-b ToolName = %q, want %q", b.ToolName, "beta")
		}

		// Clear one shouldn't affect the other
		_ = store.Clear("thread-a")
		a, _ = store.Get("thread-a")
		b, _ = store.Get("thread-b")
		if a != nil {
			t.Error("thread-a should be nil after clear")
		}
		if b == nil || b.ToolName != "beta" {
			t.Error("thread-b should be unaffected by clearing thread-a")
		}
	})

	t.Run("NilDetails", func(t *testing.T) {
		_ = store.Set("thread-nil", &PendingInterrupt{
			ToolName: "simple",
			Details:  nil,
		})

		got, err := store.Get("thread-nil")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ToolName != "simple" {
			t.Errorf("ToolName = %q, want %q", got.ToolName, "simple")
		}
	})

	t.Run("EmptyDetails", func(t *testing.T) {
		_ = store.Set("thread-empty-details", &PendingInterrupt{
			ToolName: "empty",
			Details:  map[string]any{},
		})

		got, err := store.Get("thread-empty-details")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Details == nil {
			t.Error("expected non-nil empty map for Details")
		}
	})
}

func TestInMemoryStore(t *testing.T) {
	store := NewInMemoryStore()
	storeTests(t, store)
}

func TestInMemoryStore_Concurrent(t *testing.T) {
	store := NewInMemoryStore()
	var wg sync.WaitGroup

	// Concurrent writers
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			threadID := "thread-concurrent"
			_ = store.Set(threadID, &PendingInterrupt{
				ToolCallID: "call",
				ToolName:   "tool",
			})
			_, _ = store.Get(threadID)
			_ = store.Clear(threadID)
		}(i)
	}
	wg.Wait()

	// After all goroutines finish, store should be usable
	_ = store.Set("thread-after", &PendingInterrupt{ToolName: "ok"})
	got, err := store.Get("thread-after")
	if err != nil {
		t.Fatalf("Get after concurrent: %v", err)
	}
	if got == nil || got.ToolName != "ok" {
		t.Fatal("store should be functional after concurrent access")
	}
}

func TestInMemoryStore_ImplementsInterface(t *testing.T) {
	var _ Store = (*InMemoryStore)(nil)
}

func TestValkeyStore_ImplementsInterface(t *testing.T) {
	var _ Store = (*ValkeyStore)(nil)
}
