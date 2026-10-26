package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// All station commands in these tests go to a local stub, never a device.
func uxStationStub(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "adb")
	log := filepath.Join(t.TempDir(), "commands")
	t.Setenv("UX_STATION_LOG", log)
	t.Setenv("ADB_PATH", path)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'called\\n' >> \"$UX_STATION_LOG\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return log
}

func uxActive(q *Queue, index int) *Active {
	done := make(chan struct{})
	close(done)
	return &Active{QueueID: q.ID, EntryID: q.Tracks[index].EntryID, TrackIndex: index, StartedAt: time.Now(), Cancel: func() {}, Done: done}
}

func uxQueueCall(t *testing.T, app *App, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	app.updateQueue(w, httptest.NewRequest(http.MethodPost, "/api/queue", strings.NewReader(string(data))))
	return w
}

func TestAppendPreservesSessionAndDuplicateIdentity(t *testing.T) {
	app, _ := metadataTestApp(t)
	writeMetadataFile(t, app.cfg.MusicRoot, "2.mp3")
	q, _ := app.mergeQueueLocked([]Track{{ID: "same", Path: "first.mp3"}}, true)
	active := uxActive(q, 0)
	app.active = active
	started := active.StartedAt
	w := uxQueueCall(t, app, queueRequest{Action: "append", Paths: []string{"2.mp3", "2.mp3"}})
	if w.Code != 200 {
		t.Fatalf("append: %d %s", w.Code, w.Body.String())
	}
	if app.active != active || app.active.StartedAt != started || app.queue.ID != q.ID || app.queue.Version != q.Version+1 {
		t.Fatal("append changed playback session or queue identity")
	}
	if !shouldAutoAdvance(active, nil, app.queue) {
		t.Fatal("newly appended successor unavailable to auto-advance")
	}
	if app.queue.Tracks[1].ID != app.queue.Tracks[2].ID || app.queue.Tracks[1].EntryID == app.queue.Tracks[2].EntryID {
		t.Fatal("duplicate occurrences need distinct entry IDs and the same cache identity")
	}
	if len(q.Tracks) != 1 {
		t.Fatal("append mutated a previous queue snapshot")
	}
}

func TestQueueEditPreservesCurrentAndRejectsStaleVersion(t *testing.T) {
	app := &App{}
	q, _ := app.mergeQueueLocked([]Track{{ID: "a"}, {ID: "b"}, {ID: "c"}}, true)
	active := uxActive(q, 1)
	app.active = active
	request := queueRequest{Action: "move", QueueID: q.ID, Version: q.Version, EntryID: q.Tracks[1].EntryID, ToIndex: 2}
	w := uxQueueCall(t, app, request)
	if w.Code != 200 || app.active != active || active.TrackIndex != 2 || app.queue.Tracks[2].ID != "b" {
		t.Fatalf("move current: %d %s %+v", w.Code, w.Body.String(), active)
	}
	w = uxQueueCall(t, app, request)
	if w.Code != http.StatusConflict || app.queue.Version != q.Version+1 {
		t.Fatal("stale mutation was not rejected")
	}
	w = uxQueueCall(t, app, queueRequest{Action: "remove", QueueID: q.ID, Version: app.queue.Version, EntryID: q.Tracks[0].EntryID})
	if w.Code != 200 || active.TrackIndex != 1 || app.queue.Tracks[1].EntryID != active.EntryID || active.Stopped {
		t.Fatal("removing a preceding entry changed the playing track")
	}
	uxStationStub(t)
	app.cfg = Config{StationMode: "adb", StationSerial: "stub"}
	w = uxQueueCall(t, app, queueRequest{Action: "remove", QueueID: q.ID, Version: app.queue.Version, EntryID: active.EntryID})
	if w.Code != 200 || app.active != nil || !active.Stopped || len(app.queue.Tracks) != 1 {
		t.Fatalf("remove current did not stop playback: %d %s", w.Code, w.Body.String())
	}
}

func TestStableStreamIdentityAfterReorder(t *testing.T) {
	app := &App{token: "stub", cfg: Config{PublicBase: "http://localhost"}}
	q, _ := app.mergeQueueLocked([]Track{{ID: "a", Duration: 7}, {ID: "b", Duration: 17}}, true)
	stream := app.streamURL(app.cfg, q.ID, 0)
	w := uxQueueCall(t, app, queueRequest{Action: "move", QueueID: q.ID, Version: q.Version, EntryID: q.Tracks[0].EntryID, ToIndex: 1})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	// a is now last, so HEAD neither reads audio nor starts next-track prefetch.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /music/q/{queue}/{track}", app.serveQueue)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodHead, stream, nil))
	if w.Code != 200 || w.Header().Get("Content-Length") != "192000" {
		t.Fatalf("stream URL changed its track after reorder: %d %v", w.Code, w.Header())
	}
	w = httptest.NewRecorder()
	app.control(w, httptest.NewRequest(http.MethodPost, "/api/control", strings.NewReader(`{"action":"index","entryId":"missing"}`)))
	if w.Code != http.StatusConflict {
		t.Fatal("stale playback target should be rejected")
	}
}

type uxBrokenReader struct{}

func (uxBrokenReader) Read([]byte) (int, error) { return 0, errors.New("interrupted") }

func TestUploadAtomicCollisionFailureAndCancellation(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "track.mp3")
	if err := os.WriteFile(target, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	const count = 8
	var wg sync.WaitGroup
	results := make(chan string, count)
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path, err := saveUpload(context.Background(), target, strings.NewReader("new complete track"))
			if err != nil {
				t.Error(err)
			}
			results <- path
		}()
	}
	wg.Wait()
	close(results)
	seen := map[string]bool{}
	for path := range results {
		if seen[path] || path == target || path == "" {
			t.Fatalf("collision: %q", path)
		}
		seen[path] = true
	}
	if _, err := saveUpload(context.Background(), target, io.MultiReader(strings.NewReader("partial"), uxBrokenReader{})); err == nil {
		t.Fatal("interrupted reader succeeded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := saveUpload(ctx, target, strings.NewReader("cancelled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled upload: %v", err)
	}
	original, _ := os.ReadFile(target)
	files, _ := os.ReadDir(root)
	if string(original) != "original" || len(files) != count+1 {
		t.Fatalf("original changed or temporary/partial files left behind: %q %v", original, files)
	}
}

func TestUnchangedConfigDoesNotStopAndRootChangeClears(t *testing.T) {
	log := uxStationStub(t)
	root := t.TempDir()
	cfg := Config{MusicRoot: root, CacheDir: t.TempDir(), UploadDir: "uploads", ConfigPath: filepath.Join(t.TempDir(), "config.json"), StationMode: "adb", StationSerial: "stub"}
	if err := normalizeConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: cfg}
	q, _ := app.mergeQueueLocked([]Track{{ID: "a"}}, true)
	app.active = uxActive(q, 0)
	w := httptest.NewRecorder()
	app.updateConfig(w, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(`{}`)))
	if w.Code != 200 || app.active == nil {
		t.Fatal("unchanged settings stopped playback")
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("unchanged settings called the station")
	}
	data, _ := json.Marshal(map[string]string{"musicRoot": t.TempDir()})
	w = httptest.NewRecorder()
	app.updateConfig(w, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(string(data))))
	if w.Code != 200 || app.active != nil || app.queue != nil {
		t.Fatalf("root change did not clear playback and queue: %d %s", w.Code, w.Body.String())
	}
}

func TestSlowMetadataDoesNotLockTransport(t *testing.T) {
	app, log := metadataTestApp(t)
	writeMetadataFile(t, app.cfg.MusicRoot, "1.mp3")
	uxStationStub(t)
	app.cfg.StationMode, app.cfg.StationSerial = "adb", "stub"
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		w := httptest.NewRecorder()
		app.updateQueue(w, httptest.NewRequest(http.MethodPost, "/api/queue", strings.NewReader(`{"action":"append","paths":["1.mp3"]}`)))
		if w.Code != 200 {
			t.Error(w.Body.String())
		}
	}()
	// Wait for the fake ffprobe to enter its deliberately slow metadata read.
	deadline := time.After(time.Second)
	for {
		if _, err := os.Stat(log); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("metadata never started")
		case <-time.After(time.Millisecond):
		}
	}
	if !app.playMu.TryLock() {
		t.Fatal("metadata preparation holds the transport lock")
	}
	app.playMu.Unlock()
	w := httptest.NewRecorder()
	app.control(w, httptest.NewRequest(http.MethodPost, "/api/control", strings.NewReader(`{"action":"stop"}`)))
	if w.Code != 200 {
		t.Fatalf("stop during preparation: %d %s", w.Code, w.Body.String())
	}
	<-finished
}
