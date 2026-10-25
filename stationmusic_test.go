package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSafeRelPath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	rel, abs, err := safeRelPath(root, "song.mp3")
	if err != nil {
		t.Fatalf("valid path: %v", err)
	}
	if rel != "song.mp3" || !filepath.IsAbs(abs) {
		t.Fatalf("unexpected result rel=%q abs=%q", rel, abs)
	}

	for _, rel := range []string{"../song.mp3", "/etc/passwd", "escape/file"} {
		if _, _, err := safeRelPath(root, rel); err == nil {
			t.Fatalf("path %q was accepted", rel)
		}
	}
}

func TestParseM3ULocalFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first.mp3", filepath.Join("sub", "second.flac")} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ffprobe := filepath.Join(t.TempDir(), "ffprobe-fail")
	if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	playlist := filepath.Join(root, "list.m3u8")
	content := `#EXTM3U
#EXTINF:123,First Artist - First
first.mp3
#EXTINF:45,Second Title
#EXTART:Second Artist
#EXTALB:Second Album
sub/second.flac
/absolute/path.mp3
`
	if err := os.WriteFile(playlist, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	app := &App{ffprobe: ffprobe}
	tracks, skipped, err := app.parseM3U(root, playlist)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(tracks))
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "/absolute/path.mp3") {
		t.Fatalf("skipped = %#v", skipped)
	}
	first := tracks[0]
	if first.Title != "First" || first.Artist != "First Artist" || first.Duration != 123 {
		t.Fatalf("first = %#v", first)
	}
	second := tracks[1]
	if second.Title != "Second Title" || second.Artist != "Second Artist" || second.Album != "Second Album" || second.Duration != 45 {
		t.Fatalf("second = %#v", second)
	}
	if second.Path != "sub/second.flac" {
		t.Fatalf("second path = %q", second.Path)
	}
}

func TestSlotBytes(t *testing.T) {
	track := Track{Duration: 9}
	if got := slotBytes(track); got != 10*24000 {
		t.Fatalf("slotBytes = %d", got)
	}
}

func TestParseRange(t *testing.T) {
	tests := []struct {
		name       string
		rangeValue string
		start      int64
		end        int64
		status     int
		wantErr    bool
	}{
		{name: "full", start: 0, end: 99, status: http.StatusOK},
		{name: "rangeValue", rangeValue: "bytes=10-19", start: 10, end: 19, status: http.StatusPartialContent},
		{name: "open-end", rangeValue: "bytes=90-", start: 90, end: 99, status: http.StatusPartialContent},
		{name: "suffix", rangeValue: "bytes=-7", start: 93, end: 99, status: http.StatusPartialContent},
		{name: "oversized-end", rangeValue: "bytes=10-1000", start: 10, end: 99, status: http.StatusPartialContent},
		{name: "invalid", rangeValue: "bytes=100-", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.rangeValue != "" {
				r.Header.Set("Range", tt.rangeValue)
			}
			w := httptest.NewRecorder()
			start, end, status, ok := parseRange(w, r, 100)
			if tt.wantErr {
				if ok || w.Code != http.StatusRequestedRangeNotSatisfiable {
					t.Fatalf("expected error, got ok=%v code=%d", ok, w.Code)
				}
				return
			}
			if !ok || start != tt.start || end != tt.end || status != tt.status {
				t.Fatalf("got start=%d end=%d status=%d ok=%v", start, end, status, ok)
			}
		})
	}
}

func TestSafeUploadTarget(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	abs, rel, err := safeUploadTarget(root, "artist/album/song.flac")
	if err != nil {
		t.Fatal(err)
	}
	if rel != "artist/album/song.flac" || filepath.Dir(abs) != filepath.Join(root, "artist", "album") {
		t.Fatalf("abs=%q rel=%q", abs, rel)
	}
	if _, _, err := safeUploadTarget(root, "outside/song.flac"); err == nil {
		t.Fatal("symlink escape accepted")
	}
	if _, _, err := safeUploadTarget(root, "../song.flac"); err == nil {
		t.Fatal("traversal accepted")
	}
}

func TestNormalizeConfigKeepsUploadsInsideMusicRoot(t *testing.T) {
	dir := t.TempDir()
	newRoot := t.TempDir()
	cfg := Config{
		MusicRoot:   dir,
		UploadDir:   filepath.Join(dir, "uploads"),
		CacheDir:    filepath.Join(dir, "cache"),
		StationMode: "adb",
	}
	cfg.MusicRoot = newRoot
	if err := normalizeConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(newRoot, "uploads")
	if cfg.UploadDir != want {
		t.Fatalf("UploadDir = %q, want %q", cfg.UploadDir, want)
	}
	if !isPathWithin(cfg.MusicRoot, cfg.UploadDir) {
		t.Fatalf("upload dir %q is outside music root %q", cfg.UploadDir, cfg.MusicRoot)
	}
}

func TestStationCommandConstruction(t *testing.T) {
	t.Setenv("ADB_PATH", "")
	t.Setenv("SSH_PATH", "")
	command := "cd /system/vendor/quasar && ./maind --gst-launch test"

	adb := stationCommandContext(context.Background(), Config{
		StationMode:   "adb",
		StationSerial: "192.0.2.10:5555",
	}, command)
	if filepath.Base(adb.Path) != "adb" || strings.Join(adb.Args, " ") != fmt.Sprintf("adb -s 192.0.2.10:5555 shell %s", command) {
		t.Fatalf("adb command = %#v", adb.Args)
	}

	ssh := stationCommandContext(context.Background(), Config{
		StationMode:   "ssh",
		StationHost:   "192.0.2.10",
		StationPort:   2222,
		StationUser:   "root",
		StationSerial: "ignored",
	}, command)
	want := fmt.Sprintf("ssh -p 2222 -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new root@192.0.2.10 %s", command)
	if filepath.Base(ssh.Path) != "ssh" || strings.Join(ssh.Args, " ") != want {
		t.Fatalf("ssh command = %#v", ssh.Args)
	}
}

func TestStopRemotePlaybackCommandBusyBoxCompatible(t *testing.T) {
	if strings.Contains(stopRemotePlaybackCommand, "pkill") {
		t.Fatalf("stop command must not use pkill: %q", stopRemotePlaybackCommand)
	}
	if !strings.Contains(stopRemotePlaybackCommand, "ps | while read pid user rest") ||
		!strings.Contains(stopRemotePlaybackCommand, `case "$rest" in './maind --gst-launch'`) ||
		!strings.Contains(stopRemotePlaybackCommand, `kill -INT "$pid"`) ||
		!strings.Contains(stopRemotePlaybackCommand, `kill -9 "$pid"`) {
		t.Fatalf("unexpected stop command: %q", stopRemotePlaybackCommand)
	}
	if out, err := exec.Command("sh", "-n", "-c", stopRemotePlaybackCommand).CombinedOutput(); err != nil {
		t.Fatalf("stop command syntax: %v: %s", err, out)
	}
}

func TestStopCleansUnusedConfiguredMixerIPC(t *testing.T) {
	for _, tc := range []struct {
		name      string
		playerPID string
		creator   string
		control   int
		buffer    int
		cleanup   bool
	}{
		// cpid stays at the first ALSA creator forever; nattch is the only
		// reliable indicator that a later gst-launch process is not using it.
		{"already exited stale mixer", "", "13128", 0, 0, true},
		{"stopped player with stale creator", "424242", "13128", 0, 0, true},
		{"control in use", "424242", "13128", 1, 0, false},
		{"buffer in use", "424242", "13128", 0, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fixtures := map[string]string{
				"/etc/asound.conf": "pcm.dmix_spk {\n  ipc_key 1024\n}\n",
				"/proc/sysvipc/shm": fmt.Sprintf(
					"key shmid perms size cpid lpid nattch\n"+
						"1024 11 666 496 %s 424242 %d\n"+
						"1025 22 666 61440 %s 424242 %d\n"+
						"2048 44 666 496 424242 424242 0\n",
					tc.creator, tc.control, tc.creator, tc.buffer),
				"/proc/sysvipc/sem": "key semid perms nsems\n1024 33 666 1\n2048 55 666 1\n",
			}
			command := stopRemotePlaybackCommand
			for path, content := range fixtures {
				local := filepath.Join(dir, filepath.Base(path))
				if err := os.WriteFile(local, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				command = strings.ReplaceAll(command, path, shellQuote(local))
			}
			// Only the matching player is stopped; system maind roles are kept.
			playerLine := ""
			if tc.playerPID != "" {
				playerLine = "  echo '" + tc.playerPID + " root ./maind --gst-launch test'\n"
			}
			stubs := "ps() {\n  echo '313131 root /system/vendor/quasar/maind --mediad'\n" + playerLine + "}\n" + `kill() {
  [ "$1" != -0 ] || return 1
  echo "signal $*"
}
ipcrm() { echo "cleanup $*"; }
`
			out, err := exec.Command("sh", "-c", stubs+command).CombinedOutput()
			if err != nil {
				t.Fatalf("stop: %v: %s", err, out)
			}
			want := ""
			if tc.playerPID != "" {
				want = "signal -INT " + tc.playerPID + "\n"
			}
			if tc.cleanup {
				want += "cleanup -m 11\ncleanup -m 22\ncleanup -s 33\n"
			}
			if string(out) != want {
				t.Fatalf("stop output = %q, want %q", out, want)
			}
		})
	}
}

func TestPlaybackHealthCommandBusyBoxCompatible(t *testing.T) {
	if out, err := exec.Command("sh", "-n", "-c", playbackHealthCommand).CombinedOutput(); err != nil {
		t.Fatalf("health command syntax: %v: %s", err, out)
	}
}

func TestStationVolumeCommandBusyBoxCompatible(t *testing.T) {
	if out, err := exec.Command("sh", "-n", "-c", stationVolumeCommand).CombinedOutput(); err != nil {
		t.Fatalf("volume command syntax: %v: %s", err, out)
	}
	if out, err := exec.Command("sh", "-n", "-c", setStationVolumeCommand(40)).CombinedOutput(); err != nil {
		t.Fatalf("set volume command syntax: %v: %s", err, out)
	}
}

func TestParsePlaybackHealthOutput(t *testing.T) {
	health, ok := parsePlaybackHealthOutput(strings.Join([]string{
		"volume_limits=Limits: Playback 0 - 255",
		"volume_channel=Front Left: Playback 102 [40%] [-30.60dB]",
		"process=present",
		"pcm_state=RUNNING",
		"pcm_hw_ptr=12345",
	}, "\n"), time.Unix(1, 0))
	if !ok {
		t.Fatal("expected valid health output")
	}
	if !health.RemoteProcess || health.AudioState != "RUNNING" || health.AudioHwPtr != 12345 || health.AudioAdvancing {
		t.Fatalf("health = %#v", health)
	}
	if health.Volume == nil || health.Volume.Percent != 40 || health.Volume.Raw != 102 || health.Volume.Max != 255 {
		t.Fatalf("health volume = %#v", health.Volume)
	}

	health, ok = parsePlaybackHealthOutput(strings.Join([]string{
		"volume_limits=Limits: Playback 0 - 255",
		"volume_channel=Mono: Playback 13 [5%] [-42.00dB]",
		"process=absent",
	}, "\n"), time.Unix(1, 0))
	if !ok {
		t.Fatal("expected absent process to be valid")
	}
	if health.RemoteProcess || health.AudioState != "closed" || health.AudioAdvancing {
		t.Fatalf("absent health = %#v", health)
	}

	for _, out := range []string{"", "garbage\n", "process=present\npcm_state=RUNNING\npcm_hw_ptr=bad\n"} {
		if _, ok := parsePlaybackHealthOutput(out, time.Unix(1, 0)); ok {
			t.Fatalf("parsePlaybackHealthOutput(%q) unexpectedly succeeded", out)
		}
	}
}

func TestPlaybackHealthProgressWithFrequentPolling(t *testing.T) {
	previous := PlaybackHealth{}
	for i := 0; i < 6; i++ {
		health := playbackHealthWithProgress(PlaybackHealth{
			CheckedAt:     time.Unix(10, 0).Add(time.Duration(i) * 500 * time.Millisecond),
			RemoteProcess: true, AudioState: "RUNNING", AudioHwPtr: int64(i * 24000),
		}, previous)
		if health.AudioAdvancing != (i > 0) {
			t.Fatalf("sample %d: advancing=%v", i, health.AudioAdvancing)
		}
		previous = health
	}

	// A stopped pointer must clear a previous successful playback check.
	stuck := previous
	stuck.CheckedAt = stuck.CheckedAt.Add(500 * time.Millisecond)
	if playbackHealthWithProgress(stuck, previous).AudioAdvancing {
		t.Fatal("a stalled pointer kept the previous playing state")
	}

	for _, state := range []string{"PREPARED", "closed", "unknown"} {
		current := previous
		current.CheckedAt = current.CheckedAt.Add(500 * time.Millisecond)
		current.AudioState = state
		current.AudioHwPtr++
		if playbackHealthWithProgress(current, previous).AudioAdvancing {
			t.Fatalf("%s must not confirm playback", state)
		}
	}
}

func TestStatusRetainsCompletedPlayback(t *testing.T) {
	active := &Active{
		QueueID: "queue", TrackIndex: 0, StartedAt: time.Now(),
		Done: make(chan struct{}), Err: errors.New("pipeline failed"),
	}
	close(active.Done)
	app := &App{active: active}
	for i := 0; i < 2; i++ {
		status := app.statusResponse()
		if status.Active == nil || !status.Active.Finished || status.Active.Error != "pipeline failed" {
			t.Fatalf("completed playback disappeared: %#v", status.Active)
		}
		if app.active != active {
			t.Fatal("status discarded playback before its transition could run")
		}
	}
}

func TestPendingAutoAdvanceRespectsNewPlayback(t *testing.T) {
	old := &Active{QueueID: "queue", TrackIndex: 0}
	current := &Active{QueueID: "queue", TrackIndex: 1}
	queue := &Queue{ID: "queue", Tracks: []Track{{ID: "one"}, {ID: "two"}}}
	app := &App{queue: queue}

	// Simulate manual controls winning playMu before a pending auto-advance.
	for _, active := range []*Active{nil, current} {
		app.active = active
		app.advancePlayback(old, nil)
		if app.active != active {
			t.Fatal("pending auto-advance overrode a stop or a manual track switch")
		}
	}
}

func TestParseVolumeStatusOutput(t *testing.T) {
	volume := parseVolumeStatusOutput(strings.Join([]string{
		"volume_limits=Limits: Playback 0 - 255\r",
		"volume_channel=Front Left: Playback 102 [40%] [-30.60dB]\r",
	}, "\n"))
	if volume.Error != "" || volume.Percent != 40 || volume.Raw != 102 || volume.Max != 255 {
		t.Fatalf("volume = %#v", volume)
	}

	volume = parseVolumeStatusOutput("volume_limits=garbage\nvolume_channel=garbage\n")
	if volume.Error == "" || volume.Percent != 0 {
		t.Fatalf("invalid volume = %#v", volume)
	}
}

func TestSetVolumeValidation(t *testing.T) {
	app := &App{}
	for _, body := range []string{`{}`, `{"percent":-1}`, `{"percent":101}`} {
		r := httptest.NewRequest(http.MethodPost, "/api/volume", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		app.setVolume(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, body = %s", body, w.Code, w.Body.String())
		}
	}
}

func TestActivePlaybackState(t *testing.T) {
	now := time.Now().UTC()
	running := &Active{StartedAt: now.Add(-5 * time.Second), Done: make(chan struct{})}
	playing, state, finished := activePlaybackState(running, PlaybackHealth{
		RemoteProcess: true, AudioState: "RUNNING", AudioAdvancing: true,
	}, now)
	if !playing || state != "playing" || finished {
		t.Fatalf("running result = playing=%v state=%q finished=%v", playing, state, finished)
	}

	provisional := &Active{StartedAt: now.Add(-5 * time.Second), Done: make(chan struct{})}
	playing, state, finished = activePlaybackState(provisional, PlaybackHealth{
		RemoteProcess: true, AudioState: "RUNNING", AudioAdvancing: false,
	}, now)
	if playing || state != "no_sound" || finished {
		t.Fatalf("provisional running result = playing=%v state=%q finished=%v", playing, state, finished)
	}

	stuck := &Active{StartedAt: now.Add(-5 * time.Second), Done: make(chan struct{})}
	playing, state, finished = activePlaybackState(stuck, PlaybackHealth{
		RemoteProcess: true, AudioState: "PREPARED", AudioAdvancing: false,
	}, now)
	if playing || state != "no_sound" || finished {
		t.Fatalf("stuck result = playing=%v state=%q finished=%v", playing, state, finished)
	}

	starting := &Active{StartedAt: now.Add(-500 * time.Millisecond), Done: make(chan struct{})}
	playing, state, finished = activePlaybackState(starting, PlaybackHealth{
		RemoteProcess: true, AudioState: "PREPARED", AudioAdvancing: false,
	}, now)
	if playing || state != "starting" || finished {
		t.Fatalf("starting result = playing=%v state=%q finished=%v", playing, state, finished)
	}

	done := &Active{StartedAt: now.Add(-10 * time.Second), Done: make(chan struct{})}
	close(done.Done)
	playing, state, finished = activePlaybackState(done, PlaybackHealth{
		RemoteProcess: false, AudioState: "closed",
	}, now)
	if playing || state != "finished" || !finished {
		t.Fatalf("done result = playing=%v state=%q finished=%v", playing, state, finished)
	}
}

func TestShouldAutoAdvance(t *testing.T) {
	queue := &Queue{ID: "queue", Tracks: []Track{{ID: "one"}, {ID: "two"}}}
	active := &Active{QueueID: "queue", TrackIndex: 0}

	if !shouldAutoAdvance(active, nil, queue) {
		t.Fatal("expected a clean exit before the last track to advance")
	}
	if shouldAutoAdvance(active, errors.New("station error"), queue) {
		t.Fatal("expected a command error not to advance")
	}

	stopped := &Active{QueueID: "queue", TrackIndex: 0, Stopped: true}
	if shouldAutoAdvance(stopped, nil, queue) {
		t.Fatal("expected an explicitly stopped track not to advance")
	}

	otherQueue := &Queue{ID: "other", Tracks: queue.Tracks}
	if shouldAutoAdvance(active, nil, otherQueue) {
		t.Fatal("expected a replaced queue not to advance")
	}

	last := &Active{QueueID: "queue", TrackIndex: 1}
	if shouldAutoAdvance(last, nil, queue) {
		t.Fatal("expected no advance after the last track")
	}
}

func TestCacheStreamsWhileTranscoding(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.wav")
	if err := os.WriteFile(source, []byte("123456"), 0o644); err != nil {
		t.Fatal(err)
	}
	ffmpeg := filepath.Join(dir, "fake-ffmpeg")
	script := `#!/bin/bash
input="$7"
output="${!#}"
if [ "$output" = "-" ]; then
  printf silence
  exit 0
fi
printf 123 > "$output"
sleep .2
printf 456 >> "$output"
`
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cacheDir := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cache := NewCache(ctx, cacheDir)
	cache.ffmpeg = ffmpeg
	defer cache.Close()

	track := Track{ID: "test", Duration: 1, source: source}
	item, err := cache.Get(track)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- item.copyRange(cache, &out, 0, 5)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for progressive cache read")
	}
	if out.String() != "123456" {
		t.Fatalf("stream = %q", out.String())
	}
}

func TestLocalEndToEndWithoutStation(t *testing.T) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}

	dir := t.TempDir()
	root := filepath.Join(dir, "music")
	album := filepath.Join(root, "album")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(album, "test.wav")
	ffmpegCmd := exec.Command(ffmpeg,
		"-nostdin", "-loglevel", "error", "-f", "lavfi",
		"-i", "sine=frequency=440:duration=2", "-c:a", "pcm_s16le", source)
	if out, err := ffmpegCmd.CombinedOutput(); err != nil {
		t.Fatalf("generate wav: %v: %s", err, out)
	}
	playlist := filepath.Join(album, "list.m3u8")
	if err := os.WriteFile(playlist, []byte("#EXTM3U\n#EXTINF:2,Station Web Test\ntest.wav\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := Config{
		Address:       "127.0.0.1:8090",
		MusicRoot:     root,
		CacheDir:      filepath.Join(dir, "cache"),
		UploadDir:     filepath.Join(root, "uploads"),
		PublicBase:    "http://127.0.0.1:8090",
		StationMode:   "adb",
		StationSerial: "",
		StationPort:   2222,
		StationUser:   "root",
		ConfigPath:    filepath.Join(dir, "config.json"),
	}
	if err := normalizeConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	app := &App{
		cfg:     cfg,
		cache:   NewCache(ctx, cfg.CacheDir),
		token:   "e2e-token",
		ffprobe: ffprobe,
		ffmpeg:  ffmpeg,
	}
	defer app.cache.Close()

	call := func(name, method, target string, body string) *httptest.ResponseRecorder {
		t.Helper()
		var reader *strings.Reader
		if body == "" {
			reader = strings.NewReader("")
		} else {
			reader = strings.NewReader(body)
		}
		r := httptest.NewRequest(method, target, reader)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		switch name {
		case "library":
			app.library(w, r)
		case "open":
			app.openPath(w, r)
		case "queue":
			app.updateQueue(w, r)
		case "status":
			app.getStatus(w, r)
		default:
			t.Fatalf("unknown handler %q", name)
		}
		return w
	}
	requireOK := func(w *httptest.ResponseRecorder, name string) map[string]any {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %s", name, w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: decode response: %v", name, err)
		}
		return out
	}

	library := requireOK(call("library", http.MethodGet, "/api/library?path=.", ""), "library")
	entries, _ := library["entries"].([]any)
	foundAlbum := false
	for _, entryValue := range entries {
		entry, _ := entryValue.(map[string]any)
		if entry["name"] == "album" && entry["kind"] == "dir" {
			foundAlbum = true
		}
	}
	if !foundAlbum {
		t.Fatalf("library entries = %#v", library["entries"])
	}
	opened := requireOK(call("open", http.MethodPost, "/api/library/open", `{"path":".","recursive":true}`), "open")
	if tracks, _ := opened["tracks"].([]any); len(tracks) != 1 {
		t.Fatalf("open tracks = %#v", opened["tracks"])
	}
	queued := requireOK(call("queue", http.MethodPost, "/api/queue", `{"action":"replace","paths":["album/test.wav","album/test.wav"],"recursive":false}`), "queue")
	queueMap, _ := queued["queue"].(map[string]any)
	queueID, _ := queueMap["id"].(string)
	if queueID == "" {
		t.Fatalf("queue response = %#v", queued)
	}
	if tracks, _ := queueMap["tracks"].([]any); len(tracks) != 2 {
		t.Fatalf("queue tracks = %#v", queueMap["tracks"])
	}
	status := requireOK(call("status", http.MethodGet, "/api/status", ""), "status")
	if ready, _ := status["stationReady"].(bool); ready {
		t.Fatal("empty ADB target must not be reported as ready")
	}
	volume, _ := status["volume"].(map[string]any)
	if volume == nil || volume["error"] != "колонка не настроена" {
		t.Fatalf("status volume = %#v", status["volume"])
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /music/q/{queue}/{track}", app.serveQueue)
	r := httptest.NewRequest(http.MethodGet, "/music/q/"+queueID+"/0.mp3?token=e2e-token", nil)
	r.Header.Set("Range", "bytes=0-1023")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusPartialContent {
		t.Fatalf("stream status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "audio/mpeg" {
		t.Fatalf("stream content type = %q", got)
	}
	if got := w.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q", got)
	}
	if got := w.Header().Get("Content-Length"); got != "1024" {
		t.Fatalf("Content-Length = %q", got)
	}
	if got := w.Header().Get("Content-Range"); !strings.HasPrefix(got, "bytes 0-1023/") {
		t.Fatalf("Content-Range = %q", got)
	}
	if w.Body.Len() != 1024 {
		t.Fatalf("stream body length = %d", w.Body.Len())
	}

	// A stream URL must describe exactly one fixed-size track slot, not the
	// concatenation of every remaining queue item.
	r = httptest.NewRequest(http.MethodHead, "/music/q/"+queueID+"/0.mp3?token=e2e-token", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if got := w.Header().Get("Content-Length"); got != strconv.FormatInt(slotBytes(Track{Duration: 2}), 10) {
		t.Fatalf("full track Content-Length = %q, body = %s", got, w.Body.String())
	}

	r = httptest.NewRequest(http.MethodGet, "/music/q/"+queueID+"/0.mp3?token=wrong", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("wrong token status = %d", w.Code)
	}
}
