package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

// ErrVersionConflict is returned by Apply when IfVersion does not match the
// running version (HTTP 409 for PUT /api/config with If-Match).
var ErrVersionConflict = errors.New("config version conflict")

// ErrUnknownVersion is returned by Rollback when no history file exists.
var ErrUnknownVersion = errors.New("config version not in history")

type Options struct {
	// HistoryDir holds old versions; default is <config dir>/history.
	HistoryDir string
	// HistoryKeep is how many old versions to keep; default 20.
	HistoryKeep int
	// Debounce groups bursts of file events; default 200ms.
	Debounce time.Duration
	Logger   *slog.Logger
	// Validators run on every load and change, after the built-in checks.
	Validators []Validator
}

// Change is delivered to subscribers after a new config is swapped in.
type Change struct {
	Old, New *Config
	Diff     Diff
	Source   string
}

// Result describes an applied change.
type Result struct {
	Version int  `json:"version"`
	Diff    Diff `json:"diff"`
}

type ApplyOptions struct {
	// IfVersion, when non-zero, must equal the running version.
	IfVersion int
	// WriteFile writes the new config back to the YAML file (UI and
	// rollback changes). File edits are not written back.
	WriteFile bool
	// Source names the origin for logs: "file", "api", "rollback".
	Source string
}

type state struct {
	cfg *Config
	raw []byte
}

// Store holds the running config and applies changes with the
// validate-diff-history-swap sequence of section 9.3.
type Store struct {
	path string
	opts Options
	log  *slog.Logger

	cur atomic.Pointer[state]

	mu       sync.Mutex // serializes changes; guards fields below
	subs     []func(Change)
	fileHash [sha256.Size]byte // last file content loaded or written by the store
}

// Open loads and validates the config file. The running version is the
// file's version, raised above any version already in the history so
// history files are never overwritten after a restart.
func Open(path string, opts Options) (*Store, error) {
	if opts.HistoryDir == "" {
		opts.HistoryDir = filepath.Join(filepath.Dir(path), "history")
	}
	if opts.HistoryKeep <= 0 {
		opts.HistoryKeep = 20
	}
	if opts.Debounce <= 0 {
		opts.Debounce = 200 * time.Millisecond
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	cfg, raw, err := Load(path, opts.Validators...)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, opts: opts, log: opts.Logger, fileHash: sha256.Sum256(raw)}
	versions, err := s.historyVersions()
	if err != nil {
		return nil, err
	}
	if cfg.Version < 1 {
		cfg.Version = 1
	}
	if n := len(versions); n > 0 && cfg.Version <= versions[n-1] {
		cfg.Version = versions[n-1] + 1
	}
	s.cur.Store(&state{cfg: cfg, raw: raw})
	return s, nil
}

// Current returns the running config. Callers must not modify it.
func (s *Store) Current() *Config { return s.cur.Load().cfg }

// Raw returns the YAML of the running config.
func (s *Store) Raw() []byte { return bytes.Clone(s.cur.Load().raw) }

// Subscribe registers fn to run after every applied change, in order, while
// the store lock is held: fn must not call Apply.
func (s *Store) Subscribe(fn func(Change)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, fn)
}

// Apply validates data and, if it differs from the running config, saves
// the old version to the history, swaps the new one in and notifies
// subscribers. On a validation error the running config is kept.
func (s *Store) Apply(data []byte, o ApplyOptions) (*Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyLocked(data, o)
}

func (s *Store) applyLocked(data []byte, o ApplyOptions) (*Result, error) {
	cur := s.cur.Load()
	if o.IfVersion != 0 && o.IfVersion != cur.cfg.Version {
		return nil, fmt.Errorf("%w: running version is %d, not %d", ErrVersionConflict, cur.cfg.Version, o.IfVersion)
	}

	cfg, err := ParseWith(data, s.opts.Validators...)
	if err != nil {
		s.log.Warn("config rejected", "source", o.Source, "error", err)
		return nil, err
	}
	diff := Compare(cur.cfg, cfg)
	if diff.Empty() {
		return &Result{Version: cur.cfg.Version, Diff: diff}, nil
	}
	cfg.Version = cur.cfg.Version + 1

	if err := s.saveHistory(cur); err != nil {
		return nil, fmt.Errorf("save config history: %w", err)
	}

	raw := data
	if o.WriteFile {
		if raw, err = setVersion(data, cfg.Version); err != nil {
			return nil, err
		}
		if err := writeFileAtomic(s.path, raw); err != nil {
			return nil, fmt.Errorf("write config: %w", err)
		}
		s.fileHash = sha256.Sum256(raw)
	}

	s.cur.Store(&state{cfg: cfg, raw: raw})
	s.log.Info("config applied", "version", cfg.Version, "source", o.Source,
		"sections", diff.Sections, "restart_required", diff.RestartRequired)

	ch := Change{Old: cur.cfg, New: cfg, Diff: diff, Source: o.Source}
	for _, fn := range s.subs {
		fn(ch)
	}
	return &Result{Version: cfg.Version, Diff: diff}, nil
}

// HistoryEntry is one saved version.
type HistoryEntry struct {
	Version int       `json:"version"`
	SavedAt time.Time `json:"saved_at"`
}

// History lists saved versions, newest first.
func (s *Store) History() ([]HistoryEntry, error) {
	versions, err := s.historyVersions()
	if err != nil {
		return nil, err
	}
	out := make([]HistoryEntry, 0, len(versions))
	for _, v := range slices.Backward(versions) {
		fi, err := os.Stat(s.historyFile(v))
		if err != nil {
			continue
		}
		out = append(out, HistoryEntry{Version: v, SavedAt: fi.ModTime()})
	}
	return out, nil
}

// Rollback applies a saved version as a new version and writes it to the file.
func (s *Store) Rollback(version int) (*Result, error) {
	data, err := os.ReadFile(s.historyFile(version))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %d", ErrUnknownVersion, version)
	}
	if err != nil {
		return nil, err
	}
	return s.Apply(data, ApplyOptions{WriteFile: true, Source: "rollback"})
}

// Watch reloads the config when the file changes on disk, until ctx ends.
// It watches the directory so editors that replace the file are seen.
func (s *Store) Watch(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	if err := w.Add(filepath.Dir(s.path)); err != nil {
		return err
	}
	target := filepath.Clean(s.path)

	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if filepath.Clean(ev.Name) == target && ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) != 0 {
				timer.Reset(s.opts.Debounce)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			s.log.Warn("config watcher error", "error", err)
		case <-timer.C:
			s.reloadFromFile()
		}
	}
}

func (s *Store) reloadFromFile() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		// Mid-replace by an editor; the next event triggers another try.
		s.log.Debug("config file not readable", "error", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := sha256.Sum256(data)
	if h == s.fileHash {
		return // our own write, or no real change
	}
	if _, err := s.applyLocked(data, ApplyOptions{Source: "file"}); err == nil {
		s.fileHash = h
	}
}

func (s *Store) historyFile(version int) string {
	return filepath.Join(s.opts.HistoryDir, "v"+strconv.Itoa(version)+".yaml")
}

// historyVersions returns saved versions in ascending order.
func (s *Store) historyVersions() ([]int, error) {
	entries, err := os.ReadDir(s.opts.HistoryDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var versions []int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "v") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		if v, err := strconv.Atoi(strings.TrimSuffix(name[1:], ".yaml")); err == nil {
			versions = append(versions, v)
		}
	}
	slices.Sort(versions)
	return versions, nil
}

func (s *Store) saveHistory(st *state) error {
	if err := os.MkdirAll(s.opts.HistoryDir, 0o755); err != nil {
		return err
	}
	raw, err := setVersion(st.raw, st.cfg.Version)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.historyFile(st.cfg.Version), raw); err != nil {
		return err
	}
	versions, err := s.historyVersions()
	if err != nil {
		return err
	}
	for len(versions) > s.opts.HistoryKeep {
		if err := os.Remove(s.historyFile(versions[0])); err != nil {
			return err
		}
		versions = versions[1:]
	}
	return nil
}

// setVersion rewrites the top-level version field, keeping comments.
func setVersion(data []byte, version int) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("config root is not a mapping")
	}
	m := root.Content[0]
	value := strconv.Itoa(version)
	found := false
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == "version" {
			m.Content[i+1].Value, m.Content[i+1].Tag = value, "!!int"
			found = true
			break
		}
	}
	if !found {
		m.Content = append([]*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "version"},
			{Kind: yaml.ScalarNode, Tag: "!!int", Value: value},
		}, m.Content...)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeFileAtomic writes via a temp file and rename so readers never see a
// partial file.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
