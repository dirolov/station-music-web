package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type publicConfig struct {
	Config
	PublicBaseResolved string `json:"publicBaseResolved"`
	StationReady       bool   `json:"stationReady"`
}

type openPathRequest struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive"`
}

type openPathResponse struct {
	Path    string   `json:"path"`
	Tracks  []Track  `json:"tracks"`
	Skipped []string `json:"skipped"`
}

type queueRequest struct {
	Action    string   `json:"action"`
	Paths     []string `json:"paths"`
	Recursive bool     `json:"recursive"`
	URL       string   `json:"url"`
	Title     string   `json:"title"`
	Live      bool     `json:"live"`
	QueueID   string   `json:"queueId"`
	Version   uint64   `json:"version"`
	EntryID   string   `json:"entryId"`
	ToIndex   int      `json:"toIndex"`
}

type controlRequest struct {
	Action  string `json:"action"`
	Index   int    `json:"index"`
	EntryID string `json:"entryId"`
}

type volumeRequest struct {
	Percent *int `json:"percent"`
}

type activeStatus struct {
	Playing        bool      `json:"playing"`
	State          string    `json:"state,omitempty"`
	QueueID        string    `json:"queueId,omitempty"`
	EntryID        string    `json:"entryId,omitempty"`
	TrackIndex     int       `json:"trackIndex"`
	EstimatedIndex int       `json:"estimatedIndex"`
	StartedAt      time.Time `json:"startedAt,omitempty"`
	Error          string    `json:"error,omitempty"`
	StationError   string    `json:"stationError,omitempty"`
	Finished       bool      `json:"finished"`
	RemoteProcess  bool      `json:"remoteProcess"`
	AudioState     string    `json:"audioState,omitempty"`
	AudioHwPtr     int64     `json:"audioHwPtr"`
	AudioAdvancing bool      `json:"audioAdvancing"`
}

type statusResponse struct {
	Queue              *Queue        `json:"queue"`
	Active             *activeStatus `json:"active"`
	Volume             VolumeStatus  `json:"volume"`
	PublicBaseResolved string        `json:"publicBaseResolved"`
	StationReady       bool          `json:"stationReady"`
	StationMode        string        `json:"stationMode"`
	StationTarget      string        `json:"stationTarget"`
}

func (a *App) getConfig(w http.ResponseWriter, r *http.Request) {
	cfg := a.config()
	writeJSON(w, http.StatusOK, makePublicConfig(cfg))
}

func makePublicConfig(cfg Config) publicConfig {
	return publicConfig{
		Config:             cfg,
		PublicBaseResolved: resolvePublicBase(cfg),
		StationReady:       validateStationTarget(cfg) == nil,
	}
}

func (a *App) updateConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Address       *string `json:"address"`
		MusicRoot     *string `json:"musicRoot"`
		PublicBase    *string `json:"publicBase"`
		StationMode   *string `json:"stationMode"`
		StationSerial *string `json:"stationSerial"`
		StationHost   *string `json:"stationHost"`
		StationPort   *int    `json:"stationPort"`
		StationUser   *string `json:"stationUser"`
	}
	if err := decodeJSON(w, r, &req, 64*1024); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	a.playMu.Lock()
	defer a.playMu.Unlock()

	previous := a.config()
	cfg := previous
	if req.Address != nil {
		cfg.Address = *req.Address
	}
	if req.MusicRoot != nil {
		cfg.MusicRoot = *req.MusicRoot
	}
	if req.PublicBase != nil {
		cfg.PublicBase = *req.PublicBase
	}
	if req.StationMode != nil {
		cfg.StationMode = *req.StationMode
	}
	if req.StationSerial != nil {
		cfg.StationSerial = *req.StationSerial
	}
	if req.StationHost != nil {
		cfg.StationHost = *req.StationHost
	}
	if req.StationPort != nil {
		cfg.StationPort = *req.StationPort
	}
	if req.StationUser != nil {
		cfg.StationUser = *req.StationUser
	}
	if err := normalizeConfig(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if cfg == previous {
		writeJSON(w, http.StatusOK, makePublicConfig(cfg))
		return
	}
	rootChanged := cfg.MusicRoot != previous.MusicRoot
	if err := saveConfig(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("save config: %w", err))
		return
	}

	a.stopPlaybackLocked(true)
	if rootChanged {
		a.queueMu.Lock()
		a.queue = nil
		a.queueMu.Unlock()
	}
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()
	writeJSON(w, http.StatusOK, makePublicConfig(cfg))
}

func (a *App) library(w http.ResponseWriter, r *http.Request) {
	resp, err := a.listLibrary(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *App) openPath(w http.ResponseWriter, r *http.Request) {
	var req openPathRequest
	if err := decodeJSON(w, r, &req, 64*1024); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Path == "" {
		req.Path = "."
	}
	tracks, skipped, err := a.expandPath(req.Path, req.Recursive)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, openPathResponse{Path: req.Path, Tracks: tracks, Skipped: skipped})
}

func (a *App) getQueue(w http.ResponseWriter, r *http.Request) {
	a.queueMu.Lock()
	q := cloneQueue(a.queue)
	a.queueMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"queue": q})
}

func (a *App) updateQueue(w http.ResponseWriter, r *http.Request) {
	var req queueRequest
	if err := decodeJSON(w, r, &req, 1024*1024); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Action == "remove" || req.Action == "move" {
		a.editQueue(w, req)
		return
	}
	if req.Action != "append" && req.Action != "replace" && req.Action != "" {
		writeError(w, http.StatusBadRequest, errors.New("unknown queue action"))
		return
	}
	root := a.config().MusicRoot
	if (len(req.Paths) == 0) == (strings.TrimSpace(req.URL) == "") {
		writeError(w, http.StatusBadRequest, errors.New("укажите файлы или одну прямую HTTP/HTTPS-ссылку"))
		return
	}
	var tracks []Track
	var skipped []string
	var err error
	if strings.TrimSpace(req.URL) != "" {
		var track Track
		track, err = a.remoteTrack(req.URL, req.Title, req.Live)
		if err == nil {
			tracks = []Track{track}
		}
	} else {
		tracks, skipped, err = a.expandPaths(req.Paths, req.Recursive)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(tracks) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("no playable tracks found"))
		return
	}

	a.playMu.Lock()
	defer a.playMu.Unlock()
	if r.Context().Err() != nil {
		return
	}
	if a.config().MusicRoot != root {
		writeError(w, http.StatusConflict, errors.New("библиотека изменилась; повторите добавление"))
		return
	}
	appendOnly := req.Action == "append"
	if !appendOnly {
		a.stopPlaybackLocked(false)
	}
	q, added := a.mergeQueueLocked(tracks, appendOnly)

	writeJSON(w, http.StatusOK, map[string]any{"queue": q, "skipped": skipped, "added": added})
}

func cloneQueue(q *Queue) *Queue {
	if q == nil {
		return nil
	}
	copy := *q
	copy.Tracks = append([]Track(nil), q.Tracks...)
	return &copy
}

// Caller holds playMu. Copy-on-write keeps response and stream snapshots immutable.
func (a *App) mergeQueueLocked(tracks []Track, appendOnly bool) (*Queue, int) {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	q := cloneQueue(a.queue)
	if !appendOnly || q == nil {
		q = &Queue{ID: newQueueID(), CreatedAt: time.Now().UTC()}
	}
	added := min(len(tracks), max(0, maxQueueTracks-len(q.Tracks)))
	for _, track := range tracks[:added] {
		track.EntryID = "e" + randomHex(12)
		q.Tracks = append(q.Tracks, track)
	}
	if added > 0 {
		q.Version++
	}
	a.queue = q
	return cloneQueue(q), added
}

func (a *App) editQueue(w http.ResponseWriter, req queueRequest) {
	a.playMu.Lock()
	defer a.playMu.Unlock()
	a.queueMu.Lock()
	if a.queue == nil || a.queue.ID != req.QueueID || a.queue.Version != req.Version {
		a.queueMu.Unlock()
		writeError(w, http.StatusConflict, errors.New("очередь изменилась; обновите её и повторите действие"))
		return
	}
	q := cloneQueue(a.queue)
	index := -1
	for i, track := range q.Tracks {
		if track.EntryID == req.EntryID && req.EntryID != "" {
			index = i
			break
		}
	}
	if index < 0 || (req.Action == "move" && (req.ToIndex < 0 || req.ToIndex >= len(q.Tracks))) {
		a.queueMu.Unlock()
		writeError(w, http.StatusBadRequest, errors.New("трек или позиция не найдены"))
		return
	}
	active := a.active
	currentID := ""
	if active != nil && active.TrackIndex < len(q.Tracks) {
		currentID = q.Tracks[active.TrackIndex].EntryID
	}
	removeCurrent := req.Action == "remove" && currentID == req.EntryID
	a.queueMu.Unlock()
	if removeCurrent {
		a.stopPlaybackLocked(true)
	}
	a.queueMu.Lock()
	track := q.Tracks[index]
	q.Tracks = append(q.Tracks[:index], q.Tracks[index+1:]...)
	if req.Action == "move" {
		q.Tracks = append(q.Tracks, Track{})
		copy(q.Tracks[req.ToIndex+1:], q.Tracks[req.ToIndex:])
		q.Tracks[req.ToIndex] = track
	}
	q.Version++
	a.queue = q
	if a.active == active && active != nil {
		for i, track := range q.Tracks {
			if track.EntryID == currentID {
				active.TrackIndex = i
				break
			}
		}
	}
	a.queueMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"queue": q})
}

func (a *App) clearQueue(w http.ResponseWriter, r *http.Request) {
	a.playMu.Lock()
	a.stopPlaybackLocked(true)
	a.queueMu.Lock()
	a.queue = nil
	a.queueMu.Unlock()
	a.playMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"queue": nil})
}

func (a *App) upload(w http.ResponseWriter, r *http.Request) {
	cfg := a.config()
	r.Body = http.MaxBytesReader(w, r.Body, 4<<30)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("parse multipart: %w", err))
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("no files uploaded"))
		return
	}
	var relativePaths []string
	if raw := r.FormValue("relativePaths"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &relativePaths); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("relativePaths: %w", err))
			return
		}
	}
	if len(relativePaths) > 0 && len(relativePaths) != len(files) {
		writeError(w, http.StatusBadRequest, errors.New("relativePaths and files lengths differ"))
		return
	}

	var candidates []trackCandidate
	var saved, skipped, renamed []string
	for i, header := range files {
		if r.Context().Err() != nil {
			return
		}
		rel := filepath.Base(header.Filename)
		if i < len(relativePaths) {
			rel = relativePaths[i]
		}
		if !isAudioPath(rel) {
			continue
		}
		abs, cleanRel, err := safeUploadTarget(cfg.UploadDir, rel)
		if err != nil {
			skipped = append(skipped, rel+": "+err.Error())
			continue
		}
		src, err := header.Open()
		if err != nil {
			skipped = append(skipped, rel+": "+err.Error())
			continue
		}
		actual, err := saveUpload(r.Context(), abs, src)
		_ = src.Close()
		if err != nil {
			skipped = append(skipped, rel+": "+err.Error())
			continue
		}
		if actual != abs {
			cleanRel, _ = filepath.Rel(cfg.UploadDir, actual)
			cleanRel = filepath.ToSlash(cleanRel)
			renamed = append(renamed, cleanRel)
		}
		candidates = append(candidates, trackCandidate{source: actual, display: cleanRel})
		saved = append(saved, cleanRel)
	}
	tracks, unreadable := a.resolveCandidates(cfg.MusicRoot, candidates)
	skipped = append(skipped, unreadable...)
	addToQueue := r.FormValue("addToQueue") != "false"
	var q *Queue
	added := 0
	if r.Context().Err() != nil {
		return
	}
	a.playMu.Lock()
	if a.config().MusicRoot != cfg.MusicRoot {
		skipped = append(skipped, "Библиотека изменилась. Файлы сохранены в прежней папке, но не добавлены в очередь.")
	} else if addToQueue && len(tracks) > 0 {
		q, added = a.mergeQueueLocked(tracks, true)
	}
	a.playMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"saved": saved, "tracks": tracks, "queue": q, "added": added, "skipped": skipped, "renamed": renamed})
}

// Publish a fully written temporary file without ever truncating an existing one.
// Link is atomic and exclusive; concurrent uploads safely get different names.
func saveUpload(ctx context.Context, target string, src io.Reader) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".station-upload-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	_, copyErr := io.Copy(tmp, src)
	if copyErr == nil {
		copyErr = tmp.Chmod(0o644)
	}
	closeErr := tmp.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ext := filepath.Ext(target)
	stem := strings.TrimSuffix(target, ext)
	for n := 0; ; n++ {
		candidate := target
		if n > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, n, ext)
		}
		if err := os.Link(tmp.Name(), candidate); err == nil {
			return candidate, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
}

func (a *App) getStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.statusResponse())
}

func (a *App) statusResponse() statusResponse {
	a.queueMu.Lock()
	var q *Queue
	if a.queue != nil {
		qCopy := *a.queue
		qCopy.Tracks = append([]Track(nil), a.queue.Tracks...)
		q = &qCopy
	}
	active := a.active
	var activeCopy *Active
	previousHealth := PlaybackHealth{}
	if active != nil {
		previousHealth = active.Health
		copy := *active
		activeCopy = &copy
	}
	var activeOut *activeStatus
	a.queueMu.Unlock()

	cfg := a.config()
	health := PlaybackHealth{CheckedAt: time.Now().UTC(), AudioState: "unknown", Volume: &VolumeStatus{}}
	if validateStationTarget(cfg) != nil {
		health.StationError = "колонка не настроена"
		health.Volume.Error = "колонка не настроена"
	} else {
		health = a.probePlaybackHealth(previousHealth)
	}

	if active != nil {
		playing, state, finished := activePlaybackState(activeCopy, health, time.Now())

		a.queueMu.Lock()
		if a.active == active {
			if health.CheckedAt.After(active.Health.CheckedAt) {
				active.Health = health
			}
		}
		err := active.Err
		a.queueMu.Unlock()

		estimated := activeCopy.TrackIndex
		if q != nil && q.ID == active.QueueID {
			estimated = estimateTrackIndex(*q, activeCopy, time.Now())
		}
		errText := ""
		if err != nil {
			errText = err.Error()
		}
		activeOut = &activeStatus{
			Playing:        playing,
			State:          state,
			QueueID:        active.QueueID,
			EntryID:        activeCopy.EntryID,
			TrackIndex:     activeCopy.TrackIndex,
			EstimatedIndex: estimated,
			StartedAt:      active.StartedAt,
			Error:          errText,
			StationError:   health.StationError,
			Finished:       finished,
			RemoteProcess:  health.RemoteProcess,
			AudioState:     health.AudioState,
			AudioHwPtr:     health.AudioHwPtr,
			AudioAdvancing: health.AudioAdvancing,
		}
	}

	target := cfg.StationSerial
	if cfg.StationMode == "ssh" {
		target = fmt.Sprintf("%s@%s:%d", cfg.StationUser, cfg.StationHost, cfg.StationPort)
	}
	return statusResponse{
		Queue:              q,
		Active:             activeOut,
		Volume:             *health.Volume,
		PublicBaseResolved: resolvePublicBase(cfg),
		StationReady:       validateStationTarget(cfg) == nil,
		StationMode:        cfg.StationMode,
		StationTarget:      target,
	}
}

func (a *App) getVolume(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.volumeStatus())
}

func (a *App) setVolume(w http.ResponseWriter, r *http.Request) {
	var req volumeRequest
	if err := decodeJSON(w, r, &req, 16*1024); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Percent == nil {
		writeError(w, http.StatusBadRequest, errors.New("percent is required"))
		return
	}
	percent := *req.Percent
	if percent < 0 || percent > 100 {
		writeError(w, http.StatusBadRequest, errors.New("percent must be between 0 and 100"))
		return
	}

	cfg := a.config()
	if err := validateStationTarget(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	a.volumeMu.Lock()
	defer a.volumeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := runStationCommandOutput(ctx, cfg, setStationVolumeCommand(percent))
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("set station volume: %w", err))
		return
	}
	volume := parseVolumeStatusOutput(out)
	a.lastVolume = &volume
	if volume.Error != "" {
		writeError(w, http.StatusBadGateway, errors.New(volume.Error))
		return
	}
	writeJSON(w, http.StatusOK, volume)
}

func (a *App) control(w http.ResponseWriter, r *http.Request) {
	var req controlRequest
	if err := decodeJSON(w, r, &req, 16*1024); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.Action = strings.ToLower(strings.TrimSpace(req.Action))
	a.playMu.Lock()
	defer a.playMu.Unlock()
	if req.Action == "stop" {
		a.stopPlaybackLocked(true)
		writeJSON(w, http.StatusOK, a.statusResponse())
		return
	}
	a.queueMu.Lock()
	queueLen := 0
	current := 0
	if a.queue != nil {
		queueLen = len(a.queue.Tracks)
		if req.EntryID != "" {
			req.Index = -1
			for i, track := range a.queue.Tracks {
				if track.EntryID == req.EntryID {
					req.Index = i
					break
				}
			}
		}
		if a.active != nil {
			current = estimateTrackIndex(*a.queue, a.active, time.Now())
		}
	}
	a.queueMu.Unlock()
	if req.EntryID != "" && req.Index < 0 {
		writeError(w, http.StatusConflict, errors.New("трек больше не находится в очереди; обновите её"))
		return
	}
	if queueLen == 0 {
		writeError(w, http.StatusBadRequest, errors.New("queue is empty"))
		return
	}

	var index int
	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "play":
		index = max(0, min(req.Index, queueLen-1))
	case "index":
		if req.Index < 0 || req.Index >= queueLen {
			writeError(w, http.StatusBadRequest, fmt.Errorf("index %d is out of range", req.Index))
			return
		}
		index = req.Index
	case "next":
		index = current + 1
		if index >= queueLen {
			a.stopPlaybackLocked(true)
			writeJSON(w, http.StatusOK, a.statusResponse())
			return
		}
	case "prev", "previous":
		index = max(0, current-1)
	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown action %q", req.Action))
		return
	}
	if err := a.startPlaybackAtLocked(index); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, a.statusResponse())
}

func safeUploadTarget(uploadRoot, rel string) (string, string, error) {
	rootAbs, err := filepath.Abs(uploadRoot)
	if err != nil {
		return "", "", err
	}
	rootEval, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", "", err
	}
	rel = strings.TrimSpace(rel)
	native := filepath.Clean(filepath.FromSlash(rel))
	if native == "." || native == ".." || filepath.IsAbs(native) ||
		strings.HasPrefix(native, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("unsafe upload path %q", rel)
	}
	abs := filepath.Join(rootAbs, native)
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", "", err
	}
	parentEval, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", "", err
	}
	if parentEval != rootEval && !strings.HasPrefix(parentEval, rootEval+string(filepath.Separator)) {
		return "", "", fmt.Errorf("upload path %q escapes the upload directory", rel)
	}
	if _, err := os.Stat(abs); err == nil {
		eval, evalErr := filepath.EvalSymlinks(abs)
		if evalErr != nil {
			return "", "", evalErr
		}
		if eval != rootEval && !strings.HasPrefix(eval, rootEval+string(filepath.Separator)) {
			return "", "", fmt.Errorf("upload path %q escapes the upload directory", rel)
		}
		if info, statErr := os.Stat(abs); statErr == nil && info.IsDir() {
			return "", "", fmt.Errorf("upload path %q is a directory", rel)
		}
	}
	return abs, filepath.ToSlash(native), nil
}

func newQueueID() string {
	return randomHex(12)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	return dec.Decode(dst)
}
