package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T, content string, opts Options) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mock.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func withSpread(spread string) string {
	return strings.Replace(base, "spread: 0.00010", "spread: "+spread, 1)
}

func TestStoreApplyBumpsVersionAndKeepsHistory(t *testing.T) {
	s, path := newTestStore(t, "version: 5\n"+base, Options{HistoryKeep: 2})
	if v := s.Current().Version; v != 5 {
		t.Fatalf("initial version = %d, want 5", v)
	}

	var changes []Change
	s.Subscribe(func(c Change) { changes = append(changes, c) })

	for i, spread := range []string{"0.00020", "0.00030", "0.00040"} {
		res, err := s.Apply([]byte(withSpread(spread)), ApplyOptions{WriteFile: true, Source: "api"})
		if err != nil {
			t.Fatal(err)
		}
		if want := 6 + i; res.Version != want || s.Current().Version != want {
			t.Fatalf("version = %d/%d, want %d", res.Version, s.Current().Version, want)
		}
	}
	if len(changes) != 3 || changes[0].Diff.Sections[0] != "symbols" || changes[0].Old.Version != 5 {
		t.Fatalf("unexpected changes: %+v", changes)
	}

	// The file carries the new version.
	onDisk, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Version != 8 || onDisk.Symbols[0].Spread != 0.0004 {
		t.Fatalf("file version=%d spread=%v", onDisk.Version, onDisk.Symbols[0].Spread)
	}

	// Only the latest 2 old versions remain.
	hist, err := s.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 || hist[0].Version != 7 || hist[1].Version != 6 {
		t.Fatalf("history = %+v", hist)
	}

	// Re-opening starts above the history so no version is reused.
	s2, err := Open(path, Options{HistoryDir: filepath.Join(filepath.Dir(path), "history")})
	if err != nil {
		t.Fatal(err)
	}
	if s2.Current().Version != 8 {
		t.Fatalf("reopened version = %d", s2.Current().Version)
	}
}

func TestStoreRejectsInvalidAndKeepsOld(t *testing.T) {
	s, _ := newTestStore(t, base, Options{})
	before := s.Current()
	_, err := s.Apply([]byte(withSpread("0.000015")), ApplyOptions{Source: "api"})
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("want validation error, got %v", err)
	}
	if s.Current() != before {
		t.Fatal("config replaced despite validation error")
	}
}

func TestStoreIfVersionConflict(t *testing.T) {
	s, _ := newTestStore(t, base, Options{})
	if _, err := s.Apply([]byte(withSpread("0.00020")), ApplyOptions{IfVersion: 1}); err != nil {
		t.Fatal(err)
	}
	// A second editor still holding version 1 gets a conflict.
	_, err := s.Apply([]byte(withSpread("0.00030")), ApplyOptions{IfVersion: 1})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("want ErrVersionConflict, got %v", err)
	}
}

func TestStoreNoChangeKeepsVersion(t *testing.T) {
	s, _ := newTestStore(t, base, Options{})
	res, err := s.Apply([]byte("# only a comment\n"+base), ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != 1 || !res.Diff.Empty() {
		t.Fatalf("result = %+v", res)
	}
}

func TestStoreRollback(t *testing.T) {
	s, path := newTestStore(t, base, Options{})
	if _, err := s.Apply([]byte(withSpread("0.00020")), ApplyOptions{WriteFile: true}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Rollback(1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != 3 || s.Current().Symbols[0].Spread != 0.0001 {
		t.Fatalf("after rollback version=%d spread=%v", res.Version, s.Current().Symbols[0].Spread)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "version: 3") {
		t.Fatalf("file not rewritten:\n%s", data)
	}
	if _, err := s.Rollback(99); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("want ErrUnknownVersion, got %v", err)
	}
}

func TestSetVersionKeepsComments(t *testing.T) {
	out, err := setVersion([]byte("# header\nversion: 3 # bumped by mockfix\nserver: {}\n"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); !strings.Contains(s, "# header") || !strings.Contains(s, "version: 4 # bumped by mockfix") {
		t.Fatalf("got:\n%s", s)
	}
	out, err = setVersion([]byte("server: {}\n"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "version: 2\n") {
		t.Fatalf("got:\n%s", out)
	}
}

func TestDiffRestartRequiredAndSessions(t *testing.T) {
	old, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(base, "target_comp_id: CLIENT1 }", "target_comp_id: CLIENT2 }\n    listen: 0.0.0.0:9999", 1)
	nw, err := Parse([]byte(changed))
	if err != nil {
		t.Fatal(err)
	}
	d := Compare(old, nw)
	if len(d.RestartRequired) != 1 || d.RestartRequired[0] != "server.fix.listen" {
		t.Errorf("restart_required = %v", d.RestartRequired)
	}
	if len(d.SessionsAdded) != 1 || d.SessionsAdded[0].TargetCompID != "CLIENT2" ||
		len(d.SessionsRemoved) != 1 || d.SessionsRemoved[0].TargetCompID != "CLIENT1" {
		t.Errorf("sessions added=%v removed=%v", d.SessionsAdded, d.SessionsRemoved)
	}
}

func TestStoreWatchReloadsFileEdits(t *testing.T) {
	s, path := newTestStore(t, base, Options{Debounce: 20 * time.Millisecond})

	var mu sync.Mutex
	var sources []string
	got := make(chan Change, 10)
	s.Subscribe(func(c Change) {
		mu.Lock()
		sources = append(sources, c.Source)
		mu.Unlock()
		got <- c
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Watch(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	time.Sleep(50 * time.Millisecond) // let the watcher register

	// Hand edit: picked up once, after the debounce, even with several writes.
	for range 3 {
		if err := os.WriteFile(path, []byte(withSpread("0.00020")), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case c := <-got:
		if c.Source != "file" || c.New.Symbols[0].Spread != 0.0002 || c.New.Version != 2 {
			t.Fatalf("change = %+v", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("file edit not reloaded")
	}

	// Our own write-back must not trigger a second reload.
	if _, err := s.Apply([]byte(withSpread("0.00030")), ApplyOptions{WriteFile: true, Source: "api"}); err != nil {
		t.Fatal(err)
	}
	<-got // the api change itself

	// An invalid edit is rejected and the running config stays.
	if err := os.WriteFile(path, []byte(withSpread("0.000015")), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-got:
		t.Fatalf("unexpected change from %s", c.Source)
	case <-time.After(300 * time.Millisecond):
	}
	if s.Current().Version != 3 {
		t.Fatalf("version = %d, want 3", s.Current().Version)
	}
}

func TestDiffSessionVersions(t *testing.T) {
	chdirRepoRoot(t) // spec/FIX42.xml
	old, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	add42 := strings.Replace(base, "      - { sender_comp_id: MOCKSRV, target_comp_id: CLIENT1 }\n",
		"      - { sender_comp_id: MOCKSRV, target_comp_id: CLIENT1 }\n      - { sender_comp_id: MOCKSRV, target_comp_id: C42, version: FIX.4.2 }\n", 1)
	cfg42, err := Parse([]byte(add42))
	if err != nil {
		t.Fatal(err)
	}
	d := Compare(old, cfg42)
	if len(d.RestartRequired) != 1 || !strings.Contains(d.RestartRequired[0], "C42 FIX.4.2") {
		t.Fatalf("adding a FIX 4.2 session: restart_required = %v", d.RestartRequired)
	}
	add44 := strings.Replace(add42, "version: FIX.4.2", "version: FIX.4.4", 1)
	cfg44, _ := Parse([]byte(add44))
	if d := Compare(old, cfg44); len(d.RestartRequired) != 0 {
		t.Fatalf("adding a default-version session needs no restart: %v", d.RestartRequired)
	}
	if d := Compare(cfg44, cfg42); len(d.RestartRequired) != 1 {
		t.Fatalf("changing a session's version: %v", d.RestartRequired)
	}
}
