package dupe

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path string, data []byte) string {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func video(seed byte, size int) []byte {
	b := bytes.Repeat([]byte{seed}, size)
	b[size/2] ^= 0xff
	return b
}

func TestSame(t *testing.T) {
	d := t.TempDir()
	big := 3*sample + 123
	a := write(t, filepath.Join(d, "a.mkv"), video(1, big))
	b := write(t, filepath.Join(d, "b.mkv"), video(1, big))
	other := video(1, big)
	other[big-10] = 9 // differs only in the tail sample
	c := write(t, filepath.Join(d, "c.mkv"), other)
	small := write(t, filepath.Join(d, "s.mkv"), video(1, 100))

	var cache Cache
	for _, tc := range []struct {
		x, y string
		want bool
	}{{a, b, true}, {a, c, false}, {a, small, false}, {a, a, false}} {
		got, err := cache.Same(tc.x, tc.y)
		if err != nil || got != tc.want {
			t.Errorf("Same(%s, %s) = %v, %v; want %v", filepath.Base(tc.x), filepath.Base(tc.y), got, err, tc.want)
		}
	}

	// A rewritten file is re-hashed, not served from the cache.
	write(t, b, video(2, big))
	if same, _ := cache.Same(a, b); same {
		t.Error("stale cache: rewritten file still reported identical")
	}
}

func TestFindInAndGroups(t *testing.T) {
	d := t.TempDir()
	lib := filepath.Join(d, "Shows")
	e1 := write(t, filepath.Join(lib, "X/Season 01/X S01E01.mp4"), video(1, 5000))
	e2 := write(t, filepath.Join(lib, "X/Season 01/X S01E02.mp4"), video(1, 5000))
	e3 := write(t, filepath.Join(lib, "X/Season 01/X S01E03.mp4"), video(2, 5000))
	write(t, filepath.Join(lib, ".shelf-review/X S01E09.mp4"), video(3, 5000))
	write(t, filepath.Join(lib, "X/Season 01/X S01E03.nfo"), video(3, 5000))
	in := write(t, filepath.Join(d, "in/S01E04 - X.mp4"), video(2, 5000))
	hidden := write(t, filepath.Join(d, "in/S01E09 - X.mp4"), video(3, 5000))

	var c Cache
	if got, _ := c.FindIn(in, lib); got != e3 {
		t.Errorf("FindIn = %q, want %q", got, e3)
	}
	if got, _ := c.FindIn(in, lib, e3); got != "" {
		t.Errorf("FindIn with skip = %q, want none", got)
	}
	if got, _ := c.FindIn(hidden, lib); got != "" {
		t.Errorf("FindIn entered hidden dir or matched a non-video: %q", got)
	}
	if got, err := c.FindIn(in, filepath.Join(d, "missing")); got != "" || err != nil {
		t.Errorf("missing dir: %q, %v", got, err)
	}

	g := c.Groups([]string{e1, e2, e3})
	if len(g) != 1 || len(g[0]) != 2 || g[0][0] != e1 || g[0][1] != e2 {
		t.Errorf("Groups = %v", g)
	}
}
