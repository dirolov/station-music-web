package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func metadataTestApp(t *testing.T) (*App, string) {
	t.Helper()
	root := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("PROBE_TEST_LOG", log)
	probe := filepath.Join(t.TempDir(), "ffprobe")
	// Deliberately complete files out of order to exercise ordered collection.
	script := `#!/bin/sh
for source; do :; done
printf 'start %s\n' "$source" >> "$PROBE_TEST_LOG"
case "$source" in
  *1.mp3) sleep 0.12 ;;
  *) sleep 0.03 ;;
esac
printf 'end %s\n' "$source" >> "$PROBE_TEST_LOG"
case "$source" in *broken*) exit 1 ;; esac
printf '%s\n' '{"format":{"duration":"12","tags":{"artist":"Artist"}}}'
`
	if err := os.WriteFile(probe, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return &App{cfg: Config{MusicRoot: root}, ffprobe: probe}, log
}

func writeMetadataFile(t *testing.T, root, name string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("music"), 0644); err != nil {
		t.Fatal(err)
	}
}

func probeStarts(t *testing.T, log string) int {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "start ")
}

func TestMetadataFolderParallelOrderAndCache(t *testing.T) {
	app, log := metadataTestApp(t)
	root := app.cfg.MusicRoot
	for _, name := range []string{"10.mp3", "2.mp3", "1.mp3", "3.mp3", "4.mp3", "5.mp3", "6.mp3", "broken.mp3", "sub/7.mp3"} {
		writeMetadataFile(t, root, name)
	}
	outside := filepath.Join(t.TempDir(), "escape.mp3")
	if err := os.WriteFile(outside, []byte("outside"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.mp3")); err != nil {
		t.Fatal(err)
	}
	tracks, skipped, err := app.expandPath(".", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1.mp3", "2.mp3", "3.mp3", "4.mp3", "5.mp3", "6.mp3", "10.mp3"}
	if len(tracks) != len(want) || len(skipped) != 2 {
		t.Fatalf("tracks=%v skipped=%v", tracks, skipped)
	}
	for i, name := range want {
		if tracks[i].Path != name {
			t.Fatalf("index %d: %s, want %s", i, tracks[i].Path, name)
		}
	}
	data, _ := os.ReadFile(log)
	active, peak := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "start ") {
			active++
		} else {
			active--
		}
		peak = max(peak, active)
		if active < 0 {
			t.Fatal("invalid probe event order")
		}
	}
	if active != 0 || peak < 2 || peak > metadataWorkers {
		t.Fatalf("active=%d peak=%d", active, peak)
	}
	before := probeStarts(t, log)
	if _, _, err := app.expandPath(".", false); err != nil {
		t.Fatal(err)
	}
	if got := probeStarts(t, log); got != before+1 {
		t.Fatalf("warm probes=%d, want one retry for broken file", got-before)
	}
	oldID := tracks[0].ID
	if err := os.WriteFile(filepath.Join(root, "1.mp3"), []byte("updated audio"), 0644); err != nil {
		t.Fatal(err)
	}
	track, err := app.trackFromAbs(root, filepath.Join(root, "1.mp3"), "", 0)
	if err != nil || track.ID == oldID {
		t.Fatalf("changed file: %v %v", track, err)
	}
	if got := probeStarts(t, log); got != before+2 {
		t.Fatalf("invalidation probes=%d", got-before)
	}
	recursive, _, err := app.expandPath(".", true)
	if err != nil || len(recursive) != len(want)+1 {
		t.Fatalf("recursive=%v err=%v", recursive, err)
	}
}

func TestMetadataConcurrentRequestsCoalesce(t *testing.T) {
	app, log := metadataTestApp(t)
	writeMetadataFile(t, app.cfg.MusicRoot, "1.mp3")
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := app.expandPath("1.mp3", false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := probeStarts(t, log); got != 1 {
		t.Fatalf("probes=%d want 1", got)
	}
}

func TestMetadataPlaylistFallbackIsNotCached(t *testing.T) {
	app, log := metadataTestApp(t)
	root := app.cfg.MusicRoot
	writeMetadataFile(t, root, "1.mp3")
	text := "#EXTM3U\n#EXTINF:20,First\n1.mp3\n#EXTINF:30,Second\n1.mp3\n#EXTINF:40,Broken fallback\nbroken.mp3\n/absolute.mp3\n"
	writeMetadataFile(t, root, "broken.mp3")
	path := filepath.Join(root, "list.m3u")
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	tracks, skipped, err := app.parseM3U(root, path)
	if err != nil || len(tracks) != 3 || len(skipped) != 1 {
		t.Fatalf("tracks=%v skipped=%v err=%v", tracks, skipped, err)
	}
	if tracks[0].Title != "First" || tracks[1].Title != "Second" || tracks[0].Duration != 12 || tracks[2].Title != "Broken fallback" || tracks[2].Duration != 40 {
		t.Fatalf("fallbacks: %v", tracks)
	}
	if got := probeStarts(t, log); got != 2 {
		t.Fatalf("probes=%d want 2", got)
	}
}

func TestMetadataSelectedPathsKeepRequestOrder(t *testing.T) {
	app, _ := metadataTestApp(t)
	paths := []string{"3.mp3", "1.mp3", "2.mp3"}
	for _, path := range paths {
		writeMetadataFile(t, app.cfg.MusicRoot, path)
	}
	tracks, skipped, err := app.expandPaths(paths, false)
	if err != nil || len(skipped) != 0 || len(tracks) != len(paths) {
		t.Fatalf("%v %v %v", tracks, skipped, err)
	}
	for i, path := range paths {
		if tracks[i].Path != path {
			t.Fatal(fmt.Sprintf("index %d: %s want %s", i, tracks[i].Path, path))
		}
	}
}

func TestMetadataLimiterSharedByFolderRequests(t *testing.T) {
	app, log := metadataTestApp(t)
	for _, folder := range []string{"a", "b", "c"} {
		for i := 0; i < 6; i++ {
			writeMetadataFile(t, app.cfg.MusicRoot, fmt.Sprintf("%s/%d.mp3", folder, i))
		}
	}
	tracks, _, err := app.expandPaths([]string{"b", "c", "a"}, true)
	if err != nil || len(tracks) != 18 {
		t.Fatalf("tracks=%v err=%v", tracks, err)
	}
	if tracks[0].Path != "b/0.mp3" || tracks[6].Path != "c/0.mp3" || tracks[12].Path != "a/0.mp3" {
		t.Fatalf("folder order changed: %v", tracks)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	active, peak := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "start ") {
			active++
		} else {
			active--
		}
		peak = max(peak, active)
	}
	if active != 0 || peak < 2 || peak > metadataWorkers {
		t.Fatalf("global probes: active=%d peak=%d", active, peak)
	}
}

func TestQueueReportsAcceptedCountAtLimit(t *testing.T) {
	app, _ := metadataTestApp(t)
	writeMetadataFile(t, app.cfg.MusicRoot, "1.mp3")
	app.queue = &Queue{ID: "old", Tracks: make([]Track, maxQueueTracks-1)}
	r := httptest.NewRequest(http.MethodPost, "/api/queue", strings.NewReader(`{"action":"append","paths":["1.mp3","1.mp3"]}`))
	w := httptest.NewRecorder()
	app.updateQueue(w, r)
	var result struct {
		Queue Queue `json:"queue"`
		Added int   `json:"added"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 {
		t.Fatalf("response %d: %s (%v)", w.Code, w.Body.String(), err)
	}
	if result.Added != 1 || len(result.Queue.Tracks) != maxQueueTracks {
		t.Fatalf("added=%d total=%d", result.Added, len(result.Queue.Tracks))
	}
}

func TestUploadBatchMetadataAndSkippedFiles(t *testing.T) {
	app, _ := metadataTestApp(t)
	app.cfg.UploadDir = filepath.Join(app.cfg.MusicRoot, "uploads")
	if err := os.MkdirAll(app.cfg.UploadDir, 0755); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for _, name := range []string{"1.mp3", "2.mp3", "broken.mp3"} {
		part, err := form.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte("music")); err != nil {
			t.Fatal(err)
		}
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/upload", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	w := httptest.NewRecorder()
	app.upload(w, r)
	var result struct {
		Tracks  []Track  `json:"tracks"`
		Saved   []string `json:"saved"`
		Skipped []string `json:"skipped"`
		Added   int      `json:"added"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 {
		t.Fatalf("response %d: %s (%v)", w.Code, w.Body.String(), err)
	}
	if result.Added != 2 || len(result.Saved) != 3 || len(result.Skipped) != 1 || len(result.Tracks) != 2 {
		t.Fatalf("upload result: %+v", result)
	}
	if result.Tracks[0].Path != "uploads/1.mp3" || result.Tracks[1].Path != "uploads/2.mp3" {
		t.Fatalf("upload order: %v", result.Tracks)
	}
}
