package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed web
var webFS embed.FS

var (
	token    string
	lastSeen atomic.Int64 // last time a window polled the API
)

// windowOpen reports whether a window is polling right now.
func windowOpen() bool {
	return time.Since(time.UnixMilli(lastSeen.Load())) < 6*time.Second
}

func instanceFile() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "TaskInsight", "instance.json")
}

type instance struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

// wakeRunning asks an already running copy to show its window.
func wakeRunning() bool {
	data, err := os.ReadFile(instanceFile())
	if err != nil {
		return false
	}
	var in instance
	if json.Unmarshal(data, &in) != nil || in.Port == 0 {
		return false
	}
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/show", in.Port), nil)
	req.Header.Set("X-Token", in.Token)
	resp, err := (&http.Client{Timeout: 1500 * time.Millisecond}).Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func main() {
	autostart := len(os.Args) > 1 && os.Args[1] == "--autostart"
	testFile := os.Getenv("TI_URL_FILE") // for tests: no tray, no window

	if testFile == "" && wakeRunning() {
		return
	}

	b := make([]byte, 16)
	_, _ = rand.Read(b)
	token = hex.EncodeToString(b)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("http://127.0.0.1:%d/?t=%s", port, token)

	show := func() {
		if !windowOpen() {
			openWindow(url)
		}
	}

	go sampleLoop()
	go gpuLoop()

	sub, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/stats", guard(handleStats))
	mux.HandleFunc("/api/proc", guard(handleProc))
	mux.HandleFunc("/api/action", guard(handleAction))
	mux.HandleFunc("/api/diskfiles", guard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, scanDrive(r.URL.Query().Get("drive")))
	}))
	mux.HandleFunc("/api/log", guard(handleLog))
	mux.HandleFunc("/api/openlogs", guard(func(w http.ResponseWriter, r *http.Request) {
		_ = os.MkdirAll(logDir(), 0o755)
		_ = exec.Command("explorer", logDir()).Start()
		writeJSON(w, map[string]bool{"ok": true})
	}))
	mux.HandleFunc("/api/reveal", guard(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			_ = exec.Command("explorer", "/select,"+p).Start()
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
		writeJSON(w, map[string]any{"ok": false, "message": "Файл не найден"})
	}))
	mux.HandleFunc("/api/show", guard(func(w http.ResponseWriter, r *http.Request) {
		show()
		writeJSON(w, map[string]bool{"ok": true})
	}))
	go func() { log.Fatal(http.Serve(ln, mux)) }()

	if testFile != "" {
		_ = os.WriteFile(testFile, []byte(url), 0o600)
		select {}
	}

	_ = os.MkdirAll(filepath.Dir(instanceFile()), 0o755)
	data, _ := json.Marshal(instance{Port: port, Token: token})
	_ = os.WriteFile(instanceFile(), data, 0o600)

	if !autostart {
		show()
	}
	runTray(show) // blocks until "Выход"
}

func openWindow(url string) {
	for _, p := range []string{
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
	} {
		if _, err := os.Stat(p); err == nil {
			if exec.Command(p, "--app="+url, "--window-size=1320,860").Start() == nil {
				return
			}
		}
	}
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

func guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != token {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/api/show" {
			lastSeen.Store(time.Now().UnixMilli())
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	defer mu.RUnlock()
	writeJSON(w, snap)
}

func handleLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	to := time.Now().UnixMilli()
	from := to - 3600*1000
	if v, err := strconv.ParseInt(q.Get("from"), 10, 64); err == nil {
		from = v
	}
	if v, err := strconv.ParseInt(q.Get("to"), 10, 64); err == nil {
		to = v
	}
	writeJSON(w, buildReport(from, to))
}

func handleProc(w http.ResponseWriter, r *http.Request) {
	pid, _ := strconv.Atoi(r.URL.Query().Get("pid"))
	writeJSON(w, procDetails(int32(pid)))
}

func handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	pid, _ := strconv.Atoi(r.URL.Query().Get("pid"))
	err := doAction(r.URL.Query().Get("do"), int32(pid))
	msg := "ok"
	if err != nil {
		msg = err.Error()
		if strings.Contains(strings.ToLower(msg), "access is denied") {
			msg = "Отказано в доступе — запустите TaskInsight от имени администратора"
		}
	}
	writeJSON(w, map[string]any{"ok": err == nil, "message": msg})
}

var mu sync.RWMutex
