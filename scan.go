package main

import (
	"container/heap"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const topN = 200

// Node is one directory of the scanned tree. Files are not stored, only aggregated.
type Node struct {
	Name     string
	Size     int64 // recursive
	Own      int64 // files directly inside
	Files    int64 // recursive
	Dirs     int64 // recursive
	Err      bool
	Children []*Node // sorted by Size desc once scanned
}

type FileEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Mod  int64  `json:"mod"`
}

type ExtStat struct {
	Ext   string `json:"ext"`
	Size  int64  `json:"size"`
	Count int64  `json:"count"`
}

type DirStat struct {
	Path  string `json:"path"`
	Own   int64  `json:"own"`
	Size  int64  `json:"size"`
	Files int64  `json:"files"`
}

type fileHeap []FileEntry

func (h fileHeap) Len() int           { return len(h) }
func (h fileHeap) Less(i, j int) bool { return h[i].Size < h[j].Size }
func (h fileHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *fileHeap) Push(x any)        { *h = append(*h, x.(FileEntry)) }
func (h *fileHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

type Scanner struct {
	Root    string
	root    *Node
	rootDev uint64
	sem     chan struct{}
	start   time.Time

	files, dirs, bytes, errs, elapsed atomic.Int64
	topMin                            atomic.Int64
	cancel, done                      atomic.Bool
	cur                               atomic.Value // string

	mu   sync.Mutex
	top  fileHeap
	exts map[string]*ExtStat

	// Filled by finish(), read-only afterwards.
	TopFiles []FileEntry
	TopDirs  []DirStat
	Types    []ExtStat
}

func NewScanner(root string) *Scanner {
	n := runtime.NumCPU() * 4
	if n < 16 {
		n = 16
	}
	s := &Scanner{Root: root, root: &Node{Name: root}, sem: make(chan struct{}, n), exts: map[string]*ExtStat{}, start: time.Now()}
	s.cur.Store(root)
	return s
}

func (s *Scanner) Run() {
	if info, err := os.Stat(s.Root); err == nil {
		s.rootDev = devOf(info)
	}
	s.scanDir(s.root, s.Root)
	s.finish()
	s.elapsed.Store(int64(time.Since(s.start)))
	s.done.Store(true)
}

func (s *Scanner) Elapsed() time.Duration {
	if s.done.Load() {
		return time.Duration(s.elapsed.Load())
	}
	return time.Since(s.start)
}

// countable returns file info for entries worth counting: no links, devices,
// or cloud-only placeholders (OneDrive files that are not on disk).
func countable(e fs.DirEntry) (fs.FileInfo, bool) {
	if e.Type()&(fs.ModeSymlink|fs.ModeNamedPipe|fs.ModeSocket|fs.ModeDevice|fs.ModeCharDevice) != 0 {
		return nil, false
	}
	info, err := e.Info()
	if err != nil || cloudOnly(info) {
		return nil, false
	}
	return info, true
}

func (s *Scanner) scanDir(n *Node, p string) {
	if s.cancel.Load() {
		return
	}
	f, err := os.Open(p)
	if err != nil {
		n.Err = true
		s.errs.Add(1)
		return
	}
	entries, err := f.ReadDir(-1) // unsorted, faster than os.ReadDir
	f.Close()
	if err != nil && len(entries) == 0 {
		n.Err = true
		s.errs.Add(1)
	}
	if s.dirs.Add(1)%128 == 0 {
		s.cur.Store(p)
	}

	var wg sync.WaitGroup
	var local map[string]*ExtStat
	var own, nfiles int64
	for _, e := range entries {
		// On Windows (Go >= 1.23) junctions/symlinks are never IsDir, so no loops.
		if e.IsDir() {
			cp := filepath.Join(p, e.Name())
			if s.rootDev != 0 { // Linux: stay on the same filesystem (skips /proc, /sys, other mounts)
				if info, err := e.Info(); err != nil || devOf(info) != s.rootDev {
					continue
				}
			}
			c := &Node{Name: e.Name()}
			n.Children = append(n.Children, c)
			select {
			case s.sem <- struct{}{}:
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.scanDir(c, cp)
					<-s.sem
				}()
			default:
				s.scanDir(c, cp)
			}
			continue
		}
		info, ok := countable(e)
		if !ok {
			continue
		}
		sz := info.Size()
		own += sz
		nfiles++
		if local == nil {
			local = map[string]*ExtStat{}
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		st := local[ext]
		if st == nil {
			st = &ExtStat{Ext: ext}
			local[ext] = st
		}
		st.Size += sz
		st.Count++
		if sz > s.topMin.Load() {
			s.pushTop(FileEntry{Path: filepath.Join(p, e.Name()), Size: sz, Mod: info.ModTime().Unix()})
		}
	}
	s.files.Add(nfiles)
	s.bytes.Add(own)
	if local != nil {
		s.mu.Lock()
		for k, v := range local {
			if g := s.exts[k]; g != nil {
				g.Size += v.Size
				g.Count += v.Count
			} else {
				s.exts[k] = v
			}
		}
		s.mu.Unlock()
	}

	wg.Wait()
	n.Own, n.Size, n.Files = own, own, nfiles
	for _, c := range n.Children {
		n.Size += c.Size
		n.Files += c.Files
		n.Dirs += c.Dirs + 1
	}
	sort.Slice(n.Children, func(i, j int) bool { return n.Children[i].Size > n.Children[j].Size })
}

func (s *Scanner) pushTop(fe FileEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.top) < topN {
		heap.Push(&s.top, fe)
	} else if fe.Size > s.top[0].Size {
		s.top[0] = fe
		heap.Fix(&s.top, 0)
	}
	if len(s.top) == topN {
		s.topMin.Store(s.top[0].Size)
	}
}

func (s *Scanner) finish() {
	s.TopFiles = append([]FileEntry(nil), s.top...)
	sort.Slice(s.TopFiles, func(i, j int) bool { return s.TopFiles[i].Size > s.TopFiles[j].Size })

	for _, e := range s.exts {
		if e.Ext == "" {
			e.Ext = "(sans extension)"
		}
		s.Types = append(s.Types, *e)
	}
	sort.Slice(s.Types, func(i, j int) bool { return s.Types[i].Size > s.Types[j].Size })
	if len(s.Types) > 100 {
		s.Types = s.Types[:100]
	}

	var walk func(n *Node, p string)
	walk = func(n *Node, p string) {
		if n.Own > 0 {
			s.TopDirs = append(s.TopDirs, DirStat{Path: p, Own: n.Own, Size: n.Size, Files: n.Files})
		}
		for _, c := range n.Children {
			walk(c, filepath.Join(p, c.Name))
		}
	}
	walk(s.root, s.Root)
	sort.Slice(s.TopDirs, func(i, j int) bool { return s.TopDirs[i].Own > s.TopDirs[j].Own })
	if len(s.TopDirs) > topN {
		s.TopDirs = s.TopDirs[:topN]
	}
}

// find returns the node for an absolute path inside the scanned root.
func (s *Scanner) find(p string) *Node {
	rel, err := filepath.Rel(s.Root, filepath.Clean(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	n := s.root
	if rel == "." {
		return n
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		var next *Node
		for _, c := range n.Children {
			if c.Name == part {
				next = c
				break
			}
		}
		if next == nil {
			return nil
		}
		n = next
	}
	return n
}

// normPath makes paths comparable (Windows is case-insensitive) and ends them with a separator.
func normPath(p string) string {
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	if !strings.HasSuffix(p, string(filepath.Separator)) {
		p += string(filepath.Separator)
	}
	return p
}
