package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScan(t *testing.T) {
	root := t.TempDir()
	write := func(rel string, n int) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a/one.txt", 10)
	write("a/b/two.bin", 20)
	write("c.txt", 5)

	s := NewScanner(root)
	s.Run()

	if s.root.Size != 35 || s.root.Files != 3 || s.root.Dirs != 2 {
		t.Fatalf("root = size %d files %d dirs %d, want 35/3/2", s.root.Size, s.root.Files, s.root.Dirs)
	}
	if n := s.find(filepath.Join(root, "a", "b")); n == nil || n.Size != 20 {
		t.Fatalf("find a/b = %+v", n)
	}
	if s.find(filepath.Join(root, "..")) != nil {
		t.Fatal("find outside root must be nil")
	}
	if s.TopFiles[0].Size != 20 || s.root.Children[0].Name != "a" {
		t.Fatalf("top file %+v, first child %s", s.TopFiles[0], s.root.Children[0].Name)
	}
	if s.Types[0].Ext != ".bin" {
		t.Fatalf("types %+v", s.Types)
	}
}
