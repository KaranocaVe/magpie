package sessionrouting

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestPutLookupAndAtomicPersistence(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "overrides.json")
	SetPathForTest(p)
	t.Cleanup(func() { SetPathForTest("") })
	want := Override{Agent: "codex", Session: "s1", Model: "gpt-6.2-sol", Provider: "moonup0.25", Mode: Prefer}
	if err := Put(want); err != nil {
		t.Fatal(err)
	}
	got, ok := Lookup("codex", "s1", " gpt-6.2-sol ")
	if !ok || got != want {
		t.Fatalf("Lookup = %+v, %v", got, ok)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatal("empty persistence file")
	}
	if err := Delete("codex", "s1", "gpt-6.2-sol"); err != nil {
		t.Fatal(err)
	}
	if _, ok := Lookup("codex", "s1", "gpt-6.2-sol"); ok {
		t.Fatal("override survived delete")
	}
}

func TestFailedWriteDoesNotPublish(t *testing.T) {
	d := t.TempDir()
	parent := filepath.Join(d, "parent")
	if err := os.WriteFile(parent, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	SetPathForTest(filepath.Join(parent, "overrides.json"))
	t.Cleanup(func() { SetPathForTest("") })
	o := Override{Agent: "codex", Session: "s", Model: "m", Provider: "p", Mode: Prefer}
	if err := Put(o); err == nil {
		t.Fatal("Put unexpectedly succeeded")
	}
	if _, ok := Lookup("codex", "s", "m"); ok {
		t.Fatal("failed write was published")
	}
}

func TestWriteFailureAfterInitialLoadDoesNotPublish(t *testing.T) {
	p := filepath.Join(t.TempDir(), "overrides.json")
	SetPathForTest(p)
	t.Cleanup(func() { SetPathForTest("") })
	first := Override{Agent: "codex", Session: "s", Model: "m", Provider: "p", Mode: Prefer}
	if err := Put(first); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0700); err != nil {
		t.Fatal(err)
	}
	second := Override{Agent: "codex", Session: "s", Model: "m2", Provider: "p", Mode: Strict}
	if err := Put(second); err == nil {
		t.Fatal("Put unexpectedly succeeded")
	}
	if _, ok := Lookup("codex", "s", "m2"); ok {
		t.Fatal("failed update was published")
	}
	if got, ok := Lookup("codex", "s", "m"); !ok || got != first {
		t.Fatalf("initial override changed: %+v %v", got, ok)
	}
}

func TestRestartReadback(t *testing.T) {
	p := filepath.Join(t.TempDir(), "overrides.json")
	SetPathForTest(p)
	first := Override{Agent: "codex", Session: "s", Model: "group/sol", Provider: "moonup", Mode: Strict}
	if err := Put(first); err != nil {
		t.Fatal(err)
	}
	SetPathForTest(p)
	got, ok := Lookup("codex", "s", "group/sol")
	if !ok || got != first {
		t.Fatalf("restart readback = %+v %v", got, ok)
	}
	SetPathForTest("")
}

func TestLookupMissUsesLoadedIndex(t *testing.T) {
	p := filepath.Join(t.TempDir(), "overrides.json")
	SetPathForTest(p)
	t.Cleanup(func() { SetPathForTest("") })
	o := Override{Agent: "codex", Session: "other", Model: "group/sol", Provider: "moonup", Mode: Prefer}
	if err := Put(o); err != nil {
		t.Fatal(err)
	}
	// Replace the file out of band. A hot lookup must use the published snapshot.
	o.Session = "test"
	if err := os.WriteFile(p, []byte(`{"overrides":[{"agent":"codex","session":"test","model":"group/sol","provider":"moonup","mode":"prefer"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		if _, ok := Lookup(o.Agent, o.Session, o.Model); ok {
			t.Fatal("lookup read the file instead of the published snapshot")
		}
	}); allocs != 0 {
		t.Fatalf("warm lookup miss allocated %.1f times", allocs)
	}
}

func TestConcurrentReadWrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "overrides.json")
	SetPathForTest(p)
	t.Cleanup(func() { SetPathForTest("") })
	const n = 24
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			model := fmt.Sprintf("m-%d", i)
			_ = Put(Override{Agent: "codex", Session: "s", Model: model, Provider: "p", Mode: Prefer})
			_, _ = Lookup("codex", "s", model)
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
	all, err := All()
	if err != nil || len(all) != n {
		t.Fatalf("concurrent entries = %d, %v", len(all), err)
	}
}
