package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

//go:embed web
var webFS embed.FS

type Drive struct {
	Path  string `json:"path"`
	Label string `json:"label"`
	FS    string `json:"fs"`
	Kind  string `json:"kind"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

type Proc struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
	Exe  string `json:"exe"`
	Mem  uint64 `json:"mem"`
}

var (
	scanMu sync.Mutex
	cur    *Scanner
)

func current() *Scanner {
	scanMu.Lock()
	defer scanMu.Unlock()
	return cur
}

func main() {
	port := flag.Int("port", 8765, "port HTTP (0 = aléatoire)")
	noBrowser := flag.Bool("no-browser", false, "ne pas ouvrir le navigateur")
	flag.Parse()

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		log.Fatal(err)
	}
	url := "http://" + ln.Addr().String()

	sub, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("GET /api/drives", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, listDrives()) })
	mux.HandleFunc("POST /api/scan", handleScan)
	mux.HandleFunc("POST /api/cancel", func(w http.ResponseWriter, r *http.Request) {
		if s := current(); s != nil {
			s.cancel.Store(true)
		}
		writeJSON(w, true)
	})
	mux.HandleFunc("GET /api/status", handleStatus)
	mux.HandleFunc("GET /api/dir", handleDir)
	mux.HandleFunc("GET /api/top", withDone(func(s *Scanner, r *http.Request) any { return s.TopFiles }))
	mux.HandleFunc("GET /api/topdirs", withDone(func(s *Scanner, r *http.Request) any { return s.TopDirs }))
	mux.HandleFunc("GET /api/types", withDone(func(s *Scanner, r *http.Request) any { return s.Types }))
	mux.HandleFunc("GET /api/procs", withDone(func(s *Scanner, r *http.Request) any {
		p := r.URL.Query().Get("path")
		if p == "" {
			p = s.Root
		}
		return procsUnder(listProcs(), p)
	}))

	fmt.Println("DiskScope en écoute sur", url, "(Ctrl+C pour quitter)")
	if !*noBrowser {
		openBrowser(url)
	}
	log.Fatal(http.Serve(ln, localOnly(mux)))
}

func handleScan(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimSpace(r.URL.Query().Get("path"))
	if len(p) == 2 && p[1] == ':' { // "C:" alone means "current dir on C", not the drive root
		p += `\`
	}
	p, err := filepath.Abs(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if info, err := os.Stat(p); err != nil || !info.IsDir() {
		http.Error(w, "Dossier introuvable : "+p, http.StatusBadRequest)
		return
	}
	s := NewScanner(p)
	scanMu.Lock()
	if cur != nil {
		cur.cancel.Store(true)
	}
	cur = s
	scanMu.Unlock()
	go s.Run()
	writeJSON(w, map[string]string{"root": p})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	s := current()
	if s == nil {
		writeJSON(w, map[string]any{"state": "idle"})
		return
	}
	state := "running"
	switch {
	case s.cancel.Load():
		state = "cancelled"
	case s.done.Load():
		state = "done"
	}
	writeJSON(w, map[string]any{
		"state": state, "root": s.Root, "files": s.files.Load(), "dirs": s.dirs.Load(),
		"bytes": s.bytes.Load(), "errs": s.errs.Load(), "current": s.cur.Load(),
		"elapsed": s.Elapsed().Milliseconds(),
	})
}

type childResp struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Files int64  `json:"files"`
	Dirs  int64  `json:"dirs"`
	Err   bool   `json:"err"`
	Procs int    `json:"procs"`
}

type fileResp struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	Mod  int64  `json:"mod"`
}

const maxFilesListed = 500

func handleDir(w http.ResponseWriter, r *http.Request) {
	s := current()
	if s == nil || !s.done.Load() {
		http.Error(w, "Aucun scan terminé", http.StatusConflict)
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" {
		p = s.Root
	}
	p = filepath.Clean(p)
	n := s.find(p)
	if n == nil {
		http.Error(w, "Dossier absent du scan : "+p, http.StatusNotFound)
		return
	}
	procs := listProcs()
	norms := make([]string, len(procs))
	for i, pr := range procs {
		if pr.Exe != "" {
			norms[i] = strings.TrimSuffix(normPath(pr.Exe), string(filepath.Separator))
		}
	}

	children := make([]childResp, 0, len(n.Children))
	for _, c := range n.Children {
		cp := filepath.Join(p, c.Name)
		np, cnt := normPath(cp), 0
		for _, e := range norms {
			if e != "" && strings.HasPrefix(e, np) {
				cnt++
			}
		}
		children = append(children, childResp{c.Name, cp, c.Size, c.Files, c.Dirs, c.Err, cnt})
	}

	// Files are not kept in memory: list the current folder live.
	files := []fileResp{}
	if ents, err := os.ReadDir(p); err == nil {
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			if info, ok := countable(e); ok {
				files = append(files, fileResp{e.Name(), filepath.Join(p, e.Name()), info.Size(), info.ModTime().Unix()})
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Size > files[j].Size })
	more := 0
	if len(files) > maxFilesListed {
		more = len(files) - maxFilesListed
		files = files[:maxFilesListed]
	}

	parent := ""
	if p != filepath.Clean(s.Root) {
		parent = filepath.Dir(p)
	}
	writeJSON(w, map[string]any{
		"path": p, "root": s.Root, "parent": parent, "size": n.Size, "own": n.Own,
		"files": n.Files, "dirs": n.Dirs, "err": n.Err,
		"children": children, "fileList": files, "filesMore": more,
		"procs": procsUnder(procs, p),
	})
}

func procsUnder(procs []Proc, dir string) []Proc {
	nd := normPath(dir)
	out := []Proc{}
	for _, p := range procs {
		if p.Exe != "" && strings.HasPrefix(normPath(p.Exe), nd) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mem > out[j].Mem })
	return out
}

func withDone(f func(*Scanner, *http.Request) any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := current()
		if s == nil || !s.done.Load() {
			http.Error(w, "Aucun scan terminé", http.StatusConflict)
			return
		}
		writeJSON(w, f(s, r))
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// localOnly blocks DNS-rebinding: only answer requests addressed to localhost.
func localOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		if host != "127.0.0.1" && host != "localhost" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
