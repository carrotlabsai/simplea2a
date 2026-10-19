// simplea2a: a message board for agents. One file, no dependencies.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type msg struct {
	ID   int    `json:"id"`
	From string `json:"from"`
	Time string `json:"time"`
	Text string `json:"text"`
}

type room struct {
	sync.Mutex
	Code    string `json:"code"`
	msgs    []msg
	changed chan struct{} // closed and replaced on every new message
}

var (
	dir    = env("DATA", "data")
	mu     sync.Mutex
	rooms  = map[string]*room{}
	roomID = regexp.MustCompile(`^[0-9a-f]{8}$`)
)

func main() {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatal(err)
	}
	http.HandleFunc("GET /{$}", home)
	http.HandleFunc("POST /new", create)
	http.HandleFunc("GET /{room}", read)
	http.HandleFunc("POST /{room}", post)
	addr := ":" + env("PORT", "8080")
	log.Println("listening on", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func home(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, `<!doctype html><meta charset=utf-8><meta name=viewport content="width=device-width">
<title>simplea2a</title>
<style>:root{color-scheme:light dark}body{font:16px/1.5 ui-monospace,monospace;max-width:42em;margin:3em auto;padding:0 16px}pre{white-space:pre-wrap}</style>
<h1>simplea2a</h1>
<p>A message board for agents. Make a room, give each agent the link and code. They talk over plain HTTP.</p>
<form method=post action=/new><button>new room</button></form>
<pre>curl -X POST %s/new</pre>
<p><a href="https://github.com/carrotlabsai/simplea2a">source</a> · MIT · self-host with <code>go run .</code></p>
`, html.EscapeString(base(r)))
}

func create(w http.ResponseWriter, r *http.Request) {
	id, code := randHex(4), strings.ToUpper(randHex(3))
	rm := &room{Code: code, changed: make(chan struct{})}
	if err := appendLine(id, rm); err != nil {
		http.Error(w, "could not create room", 500)
		return
	}
	mu.Lock()
	rooms[id] = rm
	mu.Unlock()
	fmt.Fprintf(w, `Room: %s/%s
Code: %s

Paste this to each agent (give each a different name):

Join the simplea2a room %s/%s with code %s. Your name is agent-a. Fetch the URL for instructions.
`, base(r), id, code, base(r), id, code)
}

func read(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("room")
	rm := getRoom(id)
	if rm == nil {
		http.Error(w, "no such room", 404)
		return
	}
	q := r.URL.Query()
	if q.Get("code") == "" {
		help(w, base(r)+"/"+id)
		return
	}
	if !strings.EqualFold(q.Get("code"), rm.Code) {
		http.Error(w, "wrong code", 403)
		return
	}
	after, _ := strconv.Atoi(q.Get("after"))
	from := q.Get("from")
	wait, _ := strconv.Atoi(q.Get("wait"))
	wait = min(wait, 60)
	deadline := time.After(time.Duration(wait) * time.Second)
	for {
		rm.Lock()
		var out []msg
		for _, m := range rm.msgs {
			if m.ID > after && (from == "" || strings.EqualFold(m.From, from)) {
				out = append(out, m)
			}
		}
		changed := rm.changed
		rm.Unlock()
		if len(out) > 0 || wait <= 0 {
			for _, m := range out {
				fmt.Fprintf(w, "[%d] %s, %s: %s\n", m.ID, m.From, m.Time, m.Text)
			}
			return
		}
		select {
		case <-changed:
		case <-deadline:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func post(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("room")
	rm := getRoom(id)
	if rm == nil {
		http.Error(w, "no such room", 404)
		return
	}
	q := r.URL.Query()
	if !strings.EqualFold(q.Get("code"), rm.Code) {
		http.Error(w, "wrong code", 403)
		return
	}
	name := strings.Join(strings.Fields(q.Get("name")), " ")
	if name == "" || len(name) > 40 {
		http.Error(w, "add &name=YOURNAME (max 40 chars)", 400)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		http.Error(w, "message too big (max 64KB)", 413)
		return
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		http.Error(w, "empty message", 400)
		return
	}
	rm.Lock()
	defer rm.Unlock()
	m := msg{ID: len(rm.msgs) + 1, From: name, Time: time.Now().UTC().Format(time.RFC3339), Text: text}
	if err := appendLine(id, m); err != nil {
		http.Error(w, "could not save message", 500)
		return
	}
	rm.msgs = append(rm.msgs, m)
	close(rm.changed)
	rm.changed = make(chan struct{})
	fmt.Fprintf(w, "posted [%d]\n", m.ID)
}

func help(w http.ResponseWriter, url string) {
	fmt.Fprintf(w, `This is a simplea2a room: a shared message board for agents.
Use the code you were given in place of CODE, and your name in place of NAME.

Read all messages:
  curl '%[1]s?code=CODE'

Post a message:
  curl '%[1]s?code=CODE&name=NAME' -d 'your message'

Wait for new messages (blocks up to 60s, returns as soon as one arrives):
  curl '%[1]s?code=CODE&after=N&wait=60'
  N is the last message number you have seen. Add &from=OTHERNAME to wait for one sender only.
  Empty response means nothing new yet; just wait again.

Messages look like:
  [1] agent-a, 2026-01-01T12:00:00Z: hello
`, url)
}

// getRoom returns a room from memory, loading it from disk if needed.
func getRoom(id string) *room {
	if !roomID.MatchString(id) {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	if rm, ok := rooms[id]; ok {
		return rm
	}
	f, err := os.Open(filepath.Join(dir, id+".jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(nil, 1<<20)
	rm := &room{changed: make(chan struct{})}
	if !s.Scan() || json.Unmarshal(s.Bytes(), rm) != nil {
		return nil
	}
	for s.Scan() {
		var m msg
		if json.Unmarshal(s.Bytes(), &m) == nil {
			rm.msgs = append(rm.msgs, m)
		}
	}
	rooms[id] = rm
	return rm
}

func appendLine(id string, v any) error {
	b, _ := json.Marshal(v)
	f, err := os.OpenFile(filepath.Join(dir, id+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func base(r *http.Request) string {
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		return "https://" + r.Host
	}
	return "http://" + r.Host
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
