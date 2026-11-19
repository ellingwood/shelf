// Package history records every file operation in an append-only JSONL log
// so operations can be listed and undone.
package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"shelf/internal/fileops"
)

type Entry struct {
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	Action    string    `json:"action"`
	Src       string    `json:"src"`
	Dst       string    `json:"dst"`
	Overwrote bool      `json:"overwrote,omitempty"`
	Rule      string    `json:"rule,omitempty"`
	OutputDir string    `json:"output_dir,omitempty"`
	UndoOf    string    `json:"undo_of,omitempty"` // set on undo records
}

type Store struct {
	path string
	mu   sync.Mutex
	seq  int
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return &Store{path: path}, nil
}

func (s *Store) newID() string {
	s.seq++
	return fmt.Sprintf("%s-%03d", time.Now().Format("20060102T150405"), s.seq%1000)
}

// Record appends an entry, assigning ID and Time.
func (s *Store) Record(e Entry) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.ID, e.Time = s.newID(), time.Now()
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return e, err
	}
	defer f.Close()
	b, _ := json.Marshal(e)
	_, err = f.Write(append(b, '\n'))
	return e, err
}

// Active returns operations that have not been undone, oldest first.
func (s *Store) Active() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var all []Entry
	undone := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.UndoOf != "" {
			undone[e.UndoOf] = true
			continue
		}
		all = append(all, e)
	}
	out := all[:0]
	for _, e := range all {
		if !undone[e.ID] {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// Undo reverses one operation and records that it was undone.
func (s *Store) Undo(e Entry) error {
	if e.Overwrote {
		return fmt.Errorf("%s overwrote an existing file; the original cannot be restored", e.Dst)
	}
	switch e.Action {
	case "move":
		if _, err := os.Lstat(e.Src); err == nil {
			return fmt.Errorf("cannot restore %s: something already exists there", e.Src)
		}
		fi, err := os.Stat(e.Dst)
		if err != nil {
			return err
		}
		if fi.IsDir() { // retrofit moves sidecar folders (e.g. .trickplay)
			if err := os.MkdirAll(filepath.Dir(e.Src), 0o775); err != nil {
				return err
			}
			if err := os.Rename(e.Dst, e.Src); err != nil {
				return err
			}
		} else if _, err := fileops.Place(e.Dst, e.Src, "move", "skip"); err != nil {
			return err
		}
	case "copy", "hardlink", "symlink", "create": // create: a downloaded subtitle
		if err := os.Remove(e.Dst); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	default:
		return fmt.Errorf("unknown action %q", e.Action)
	}
	if e.OutputDir != "" {
		fileops.RemoveEmptyDirs(filepath.Dir(e.Dst), e.OutputDir)
	}
	_, err := s.Record(Entry{Action: "undo", Src: e.Dst, Dst: e.Src, UndoOf: e.ID})
	return err
}

// Find returns the active entry with the given ID (prefix match allowed).
func (s *Store) Find(id string) (Entry, error) {
	entries, err := s.Active()
	if err != nil {
		return Entry{}, err
	}
	var found []Entry
	for _, e := range entries {
		if e.ID == id {
			return e, nil
		}
		if strings.HasPrefix(e.ID, id) {
			found = append(found, e)
		}
	}
	switch len(found) {
	case 0:
		return Entry{}, fmt.Errorf("no active history entry %q", id)
	case 1:
		return found[0], nil
	default:
		return Entry{}, fmt.Errorf("history id %q is ambiguous (%d matches)", id, len(found))
	}
}
