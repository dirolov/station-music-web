package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const probeTimeout = 15 * time.Second

type LibraryEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Kind string `json:"kind"` // dir, audio, playlist
}

type LibraryResponse struct {
	Root    string         `json:"root"`
	Path    string         `json:"path"`
	Parent  string         `json:"parent"`
	Entries []LibraryEntry `json:"entries"`
}

var audioExtensions = map[string]bool{
	".aac": true, ".flac": true, ".m4a": true, ".mp3": true,
	".oga": true, ".ogg": true, ".opus": true, ".wav": true, ".wma": true,
}

func isAudioPath(path string) bool { return audioExtensions[strings.ToLower(filepath.Ext(path))] }
func isPlaylistPath(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".m3u" || ext == ".m3u8"
}

// safeRelPath resolves a user-supplied slash-separated path below root and
// rejects absolute paths, traversal, and symlinks escaping the root.
func safeRelPath(root, rel string) (cleanRel, absPath string, err error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	rootEval, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", "", fmt.Errorf("music root: %w", err)
	}

	rel = strings.TrimSpace(rel)
	if rel == "" || rel == "." {
		rel = "."
	}
	native := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(native) || native == ".." || strings.HasPrefix(native, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path %q must be relative to the music root", rel)
	}
	abs := filepath.Join(rootAbs, native)
	eval, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", err
	}
	if eval != rootEval && !strings.HasPrefix(eval, rootEval+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path %q escapes the music root", rel)
	}
	outRel, err := filepath.Rel(rootEval, eval)
	if err != nil {
		return "", "", err
	}
	return filepath.ToSlash(outRel), eval, nil
}

func (a *App) listLibrary(rel string) (LibraryResponse, error) {
	cfg := a.config()
	cleanRel, abs, err := safeRelPath(cfg.MusicRoot, rel)
	if err != nil {
		return LibraryResponse{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return LibraryResponse{}, err
	}
	if !info.IsDir() {
		return LibraryResponse{}, fmt.Errorf("%q is not a directory", rel)
	}

	items, err := os.ReadDir(abs)
	if err != nil {
		return LibraryResponse{}, err
	}
	resp := LibraryResponse{Root: cfg.MusicRoot, Path: cleanRel, Parent: parentRel(cleanRel)}
	for _, item := range items {
		name := item.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		itemPath := filepath.Join(abs, name)
		itemInfo, err := os.Stat(itemPath)
		if err != nil {
			continue
		}
		entryRel := name
		if cleanRel != "." {
			entryRel = cleanRel + "/" + name
		}
		switch {
		case itemInfo.IsDir():
			resp.Entries = append(resp.Entries, LibraryEntry{Name: name, Path: entryRel, Kind: "dir"})
		case isPlaylistPath(name):
			resp.Entries = append(resp.Entries, LibraryEntry{Name: name, Path: entryRel, Kind: "playlist"})
		case isAudioPath(name):
			resp.Entries = append(resp.Entries, LibraryEntry{Name: name, Path: entryRel, Kind: "audio"})
		}
	}
	sort.SliceStable(resp.Entries, func(i, j int) bool {
		return naturalLess(resp.Entries[i].Name, resp.Entries[j].Name)
	})
	return resp, nil
}

func parentRel(rel string) string {
	rel = strings.Trim(rel, "/")
	if rel == "" || rel == "." {
		return "."
	}
	parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel)))
	if parent == "/" || parent == "." {
		return "."
	}
	return parent
}

func (a *App) expandPath(rel string, recursive bool) ([]Track, []string, error) {
	if isRemoteTrack(strings.TrimSpace(rel)) {
		track, err := a.trackFromURL(strings.TrimSpace(rel), "", 0)
		if err != nil {
			return nil, nil, err
		}
		return []Track{track}, nil, nil
	}
	cfg := a.config()
	cleanRel, abs, err := safeRelPath(cfg.MusicRoot, rel)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, nil, err
	}

	var tracks []Track
	var skipped []string
	switch {
	case info.IsDir():
		var candidates []trackCandidate
		err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				skipped = append(skipped, filepath.ToSlash(path))
				return nil
			}
			if d.IsDir() && !recursive && path != abs {
				return fs.SkipDir
			}
			if d.IsDir() || !isAudioPath(path) {
				return nil
			}
			candidates = append(candidates, trackCandidate{source: path, display: filepath.ToSlash(path)})
			return nil
		})
		if err != nil {
			return nil, skipped, err
		}
		sort.SliceStable(candidates, func(i, j int) bool { return naturalLess(candidates[i].source, candidates[j].source) })
		var probeSkipped []string
		tracks, probeSkipped = a.resolveCandidates(cfg.MusicRoot, candidates)
		skipped = append(skipped, probeSkipped...)
	case isAudioPath(abs):
		track, err := a.trackFromAbs(cfg.MusicRoot, abs, "", 0)
		if err != nil {
			return nil, []string{cleanRel}, err
		}
		tracks = append(tracks, track)
	case isPlaylistPath(abs):
		tracks, skipped, err = a.parseM3U(cfg.MusicRoot, abs)
		if err != nil {
			return nil, skipped, err
		}
	default:
		return nil, nil, fmt.Errorf("unsupported file type: %s", filepath.Ext(abs))
	}
	return tracks, skipped, nil
}

func (a *App) trackFromAbs(root, abs, fallbackTitle string, fallbackDuration int) (Track, error) {
	rootEval, err := filepath.EvalSymlinks(root)
	if err != nil {
		return Track{}, err
	}
	absEval, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Track{}, err
	}
	rel, err := filepath.Rel(rootEval, absEval)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return Track{}, fmt.Errorf("path %q escapes the music root", abs)
	}
	return a.makeTrack(absEval, filepath.ToSlash(rel), fallbackTitle, fallbackDuration)
}

func validateRemoteURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) > 8192 || strings.ContainsFunc(raw, unicode.IsControl) || strings.ContainsFunc(raw, unicode.IsSpace) {
		return "", fmt.Errorf("некорректная ссылка на аудио")
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" {
		return "", fmt.Errorf("нужна прямая ссылка, начинающаяся с http:// или https://")
	}
	u.Fragment = ""
	return u.String(), nil
}

func remoteTitle(source string) string {
	u, _ := url.Parse(source)
	name := filepath.Base(u.Path)
	if name == "" || name == "." || name == "/" {
		return u.Hostname()
	}
	return name
}

func (a *App) remoteTrack(raw, title string, live bool) (Track, error) {
	source, err := validateRemoteURL(raw)
	if err != nil {
		return Track{}, err
	}
	title = strings.TrimSpace(title)
	if len(title) > 300 {
		return Track{}, fmt.Errorf("название ссылки слишком длинное (максимум 300 символов)")
	}
	if live {
		if title == "" {
			title = remoteTitle(source)
		}
		sum := sha256.Sum256([]byte(source))
		return Track{ID: hex.EncodeToString(sum[:])[:24], Path: source, Title: title, Live: true, source: source}, nil
	}
	track, err := a.trackFromURL(source, "", 0)
	if err != nil {
		return Track{}, fmt.Errorf("не удалось прочитать аудио по ссылке: проверьте адрес и доступность файла; для радио включите «Прямой эфир»")
	}
	if title != "" {
		track.Title = title
	}
	return track, nil
}

func (a *App) trackFromURL(raw, fallbackTitle string, fallbackDuration int) (Track, error) {
	source, err := validateRemoteURL(raw)
	if err != nil {
		return Track{}, err
	}
	if strings.TrimSpace(fallbackTitle) == "" {
		fallbackTitle = remoteTitle(source)
	}
	track, err := a.makeTrack(source, source, fallbackTitle, fallbackDuration)
	track.Live = track.Duration <= 0
	return track, err
}

func (a *App) makeTrack(source, displayPath, fallbackTitle string, fallbackDuration int) (Track, error) {
	title := fallbackTitle
	artist := ""
	album := ""
	duration := 0
	if durationSec, tags, err := a.cachedProbe(source); err == nil {
		duration = durationSec
		if v := strings.TrimSpace(tags["title"]); v != "" {
			title = v
		}
		if v := strings.TrimSpace(tags["artist"]); v != "" {
			artist = v
		}
		if v := strings.TrimSpace(tags["album"]); v != "" {
			album = v
		}
	} else if fallbackDuration <= 0 {
		return Track{}, fmt.Errorf("ffprobe %s: %w", source, err)
	}
	if duration <= 0 {
		duration = fallbackDuration
	}
	if strings.TrimSpace(title) == "" {
		title = filepath.Base(source)
	}

	var mod string
	if info, err := os.Stat(source); err == nil {
		mod = fmt.Sprintf(":%d:%d", info.ModTime().UnixNano(), info.Size())
	}
	sum := sha256.Sum256([]byte(source + mod))
	return Track{
		ID:       hex.EncodeToString(sum[:])[:24],
		Path:     displayPath,
		Title:    title,
		Artist:   artist,
		Album:    album,
		Duration: duration,
		source:   source,
	}, nil
}

func (a *App) probeSource(source string) (int, map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	args := []string{"-v", "error", "-select_streams", "a:0", "-show_entries",
		"format=duration:format_tags=title,artist,album:stream=codec_type:stream_tags=title,artist,album",
		"-of", "json"}
	args = append(args, remoteInputOptions(source)...)
	cmd := exec.CommandContext(ctx, a.ffprobe, append(args, source)...)
	out, err := cmd.Output()
	if err != nil {
		return 0, nil, err
	}
	var parsed struct {
		Format struct {
			Duration string            `json:"duration"`
			Tags     map[string]string `json:"tags"`
		} `json:"format"`
		Streams []struct {
			Tags      map[string]string `json:"tags"`
			CodecType string            `json:"codec_type"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return 0, nil, err
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(parsed.Format.Duration), 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		if !isRemoteTrack(source) || len(parsed.Streams) == 0 || parsed.Streams[0].CodecType != "audio" {
			return 0, nil, fmt.Errorf("invalid duration %q", parsed.Format.Duration)
		}
		seconds = 0 // A verified remote audio stream can have no finite duration.
	}
	tags := map[string]string{}
	for k, v := range parsed.Format.Tags {
		tags[strings.ToLower(k)] = v
	}
	if len(parsed.Streams) > 0 {
		for k, v := range parsed.Streams[0].Tags {
			key := strings.ToLower(k)
			if _, exists := tags[key]; !exists {
				tags[key] = v
			}
		}
	}
	return int(math.Round(seconds)), tags, nil
}

func (a *App) parseM3U(root, playlistPath string) ([]Track, []string, error) {
	f, err := os.Open(playlistPath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	var candidates []trackCandidate
	extTitle, extArtist, extAlbum := "", "", ""
	extDuration := 0
	first := true
	add := func(line string) {
		line = strings.TrimSpace(line)
		c := trackCandidate{display: line, title: extTitle, artist: extArtist, album: extAlbum, duration: extDuration}
		if isRemoteTrack(line) {
			c.source = line
		} else {
			native := filepath.FromSlash(strings.Trim(line, `"`))
			if filepath.IsAbs(native) {
				// Preserve the rejection of absolute playlist paths.
				c.rejected = true
			} else {
				c.source = filepath.Join(filepath.Dir(playlistPath), native)
			}
		}
		candidates = append(candidates, c)
		extTitle, extArtist, extAlbum, extDuration = "", "", "", 0
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if first {
			line = strings.TrimPrefix(line, "\ufeff")
			first = false
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.EqualFold(trimmed, "#EXTM3U") {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(trimmed), "#EXTINF:") {
			value := strings.TrimPrefix(trimmed[8:], ":")
			durationPart, titlePart, _ := strings.Cut(value, ",")
			if n, err := strconv.Atoi(strings.TrimSpace(durationPart)); err == nil && n > 0 {
				extDuration = n
			}
			extTitle = strings.TrimSpace(titlePart)
			if artist, title, ok := strings.Cut(extTitle, " - "); ok && title != "" {
				extArtist = strings.TrimSpace(artist)
				extTitle = strings.TrimSpace(title)
			}
			continue
		}
		if strings.HasPrefix(strings.ToUpper(trimmed), "#EXTART:") {
			extArtist = strings.TrimSpace(trimmed[len("#EXTART:"):])
			continue
		}
		if strings.HasPrefix(strings.ToUpper(trimmed), "#EXTALB:") {
			extAlbum = strings.TrimSpace(trimmed[len("#EXTALB:"):])
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		add(line)
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	tracks, skipped := a.resolveCandidates(root, candidates)
	return tracks, skipped, nil
}

func (a *App) trackSource(track Track) (string, error) {
	if isRemoteTrack(track.Path) {
		return track.Path, nil
	}
	cfg := a.config()
	_, abs, err := safeRelPath(cfg.MusicRoot, track.Path)
	return abs, err
}

func naturalLess(a, b string) bool {
	for i, j := 0, 0; i < len(a) && j < len(b); {
		if unicode.IsDigit(rune(a[i])) && unicode.IsDigit(rune(b[j])) {
			i2 := i
			j2 := j
			for i2 < len(a) && unicode.IsDigit(rune(a[i2])) {
				i2++
			}
			for j2 < len(b) && unicode.IsDigit(rune(b[j2])) {
				j2++
			}
			av, aerr := strconv.Atoi(strings.TrimLeft(a[i:i2], "0"))
			bv, berr := strconv.Atoi(strings.TrimLeft(b[j:j2], "0"))
			if aerr != nil || berr != nil || av != bv {
				if aerr != nil {
					return true
				}
				if berr != nil {
					return false
				}
				return av < bv
			}
			i, j = i2, j2
			continue
		}
		if a[i] != b[j] {
			return strings.ToLower(a[:i+1]) < strings.ToLower(b[:j+1])
		}
		i++
		j++
	}
	return len(a) < len(b)
}

func isRemoteTrack(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}
