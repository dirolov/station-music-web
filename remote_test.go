package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRemoteURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
		err  bool
	}{
		{name: "http", raw: "http://example.com/a.mp3", want: "http://example.com/a.mp3"},
		{name: "https query", raw: "https://example.com/stream?token=abc", want: "https://example.com/stream?token=abc"},
		{name: "uppercase scheme", raw: "HTTPS://Example.com/a.mp3", want: "https://Example.com/a.mp3"},
		{name: "ftp", raw: "ftp://example.com/a.mp3", err: true},
		{name: "file", raw: "file:///tmp/a.mp3", err: true},
		{name: "space", raw: "http://example.com/a b.mp3", err: true},
		{name: "newline", raw: "http://example.com/a.mp3\nx", err: true},
		{name: "opaque", raw: "http:example.com", err: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateRemoteURL(tt.raw)
			if tt.err {
				if err == nil {
					t.Fatalf("accepted %q as %q", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRemoteQueueAndLiveStream(t *testing.T) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}

	dir := t.TempDir()
	wav := filepath.Join(dir, "source.wav")
	generate := exec.Command(ffmpeg, "-nostdin", "-loglevel", "error", "-f", "lavfi",
		"-i", "sine=frequency=440:duration=1", "-c:a", "pcm_s16le", wav)
	if out, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("generate wav: %v: %s", err, out)
	}
	audio, err := os.ReadFile(wav)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Content-Length", fmt.Sprint(len(audio)))
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(audio)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := Config{
		Address:       "127.0.0.1:0",
		MusicRoot:     filepath.Join(dir, "music"),
		CacheDir:      filepath.Join(dir, "cache"),
		UploadDir:     filepath.Join(dir, "music", "uploads"),
		StationMode:   "adb",
		StationSerial: "",
		ConfigPath:    filepath.Join(dir, "config.json"),
	}
	if err := normalizeConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	app := &App{
		cfg:     cfg,
		cache:   NewCache(ctx, cfg.CacheDir),
		token:   "remote-test-token",
		ffprobe: ffprobe,
		ffmpeg:  ffmpeg,
	}
	defer app.cache.Close()

	postQueue := func(body string) map[string]any {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/queue", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		app.updateQueue(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("queue status = %d, body = %s", w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	finiteURL := server.URL + "/file.wav?signature=test"
	queued := postQueue(fmt.Sprintf(`{"action":"replace","url":%q,"title":"Конечный файл"}`, finiteURL))
	validateRemoteQueue(t, queued, finiteURL, "Конечный файл", false)

	liveURL := server.URL + "/live.wav"
	queued = postQueue(fmt.Sprintf(`{"action":"replace","url":%q,"title":"Моё радио","live":true}`, liveURL))
	track := validateRemoteQueue(t, queued, liveURL, "Моё радио", true)
	liveID, _ := track["id"].(string)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /music/q/{queue}/{track}", app.serveQueue)
	streamRequest := func(method, rangeValue string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/music/q/"+queueIDValue(t, app)+"/0.mp3?token="+app.token, nil)
		if rangeValue != "" {
			r.Header.Set("Range", rangeValue)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}

	head := streamRequest(http.MethodHead, "")
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("live HEAD status=%d body=%d", head.Code, head.Body.Len())
	}
	if got := head.Header().Get("Accept-Ranges"); got != "none" {
		t.Fatalf("live Accept-Ranges = %q", got)
	}

	invalidRange := streamRequest(http.MethodGet, "bytes=5-")
	if invalidRange.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("live range status = %d", invalidRange.Code)
	}

	response := streamRequest(http.MethodGet, "")
	if response.Code != http.StatusOK || response.Body.Len() == 0 {
		t.Fatalf("live stream status=%d bytes=%d body=%q", response.Code, response.Body.Len(), response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "audio/mpeg" {
		t.Fatalf("live content type = %q", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("live cache control = %q", got)
	}
	if _, err := os.Stat(filepath.Join(cfg.CacheDir, liveID+".mp3")); !os.IsNotExist(err) {
		t.Fatalf("live stream wrote a disk cache: %v", err)
	}
}

func TestRemoteQueueRejectsInvalidInput(t *testing.T) {
	app := &App{}
	for _, body := range []string{
		`{}`,
		`{"paths":["a.mp3"],"url":"http://example.com/a.mp3"}`,
		`{"url":"ftp://example.com/a.mp3"}`,
		`{"url":"http://example.com/a.mp3","title":"` + strings.Repeat("x", 301) + `"}`,
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/queue", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		app.updateQueue(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %.80q: status = %d", body, w.Code)
		}
	}
}

func validateRemoteQueue(t *testing.T, response map[string]any, source, title string, live bool) map[string]any {
	t.Helper()
	queueMap, _ := response["queue"].(map[string]any)
	if queueMap == nil {
		t.Fatalf("queue response = %#v", response)
	}
	tracks, _ := queueMap["tracks"].([]any)
	if len(tracks) != 1 {
		t.Fatalf("queue tracks = %#v", tracks)
	}
	track, _ := tracks[0].(map[string]any)
	gotLive, _ := track["live"].(bool)
	if track["path"] != source || track["title"] != title || gotLive != live {
		t.Fatalf("track = %#v", track)
	}
	if live {
		if track["duration"] != float64(0) {
			t.Fatalf("live duration = %#v", track["duration"])
		}
	} else if track["duration"] == float64(0) {
		t.Fatalf("finite duration = %#v", track["duration"])
	}
	return track
}

func queueIDValue(t *testing.T, app *App) string {
	t.Helper()
	app.queueMu.Lock()
	defer app.queueMu.Unlock()
	if app.queue == nil {
		t.Fatal("queue is nil")
	}
	return app.queue.ID
}
