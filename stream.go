package main

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
)

func slotBytes(track Track) int64 {
	return int64(track.Duration+1) * mp3BytesPerSec
}

func (a *App) queueSnapshot(id string) (Queue, error) {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	if a.queue == nil || a.queue.ID != id || len(a.queue.Tracks) == 0 {
		return Queue{}, fmt.Errorf("queue %q not found", id)
	}
	q := *a.queue
	q.Tracks = append([]Track(nil), a.queue.Tracks...)
	return q, nil
}

func (a *App) serveQueue(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("token") != a.token {
		http.Error(w, "invalid stream token", http.StatusForbidden)
		return
	}
	queueID := r.PathValue("queue")
	if !isHex(queueID) || len(queueID) < 8 || len(queueID) > 64 {
		http.NotFound(w, r)
		return
	}
	trackValue := strings.TrimSuffix(r.PathValue("track"), ".mp3")
	q, err := a.queueSnapshot(queueID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	first, err := strconv.Atoi(trackValue)
	if strings.HasPrefix(trackValue, "e") {
		first = -1
		for i, track := range q.Tracks {
			if track.EntryID == trackValue {
				first, err = i, nil
				break
			}
		}
	}
	if err != nil || first < 0 {
		http.NotFound(w, r)
		return
	}
	if first >= len(q.Tracks) {
		http.NotFound(w, r)
		return
	}

	// GStreamer on Station Lite can wedge while crossing a virtual MP3
	// concatenation boundary. One URL must contain exactly one track; the app
	// launches the next URL after a clean process exit.
	if first+1 < len(q.Tracks) && !q.Tracks[first+1].Live {
		_, _ = a.cache.Get(q.Tracks[first+1])
	}
	track := q.Tracks[first]
	if track.Live {
		a.serveLiveTrack(w, r, track)
		return
	}
	total := slotBytes(track)
	start, end, status, ok := parseRange(w, r, total)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}

	item, cacheErr := a.cache.Get(track)
	if cacheErr != nil {
		http.Error(w, cacheErr.Error(), http.StatusInternalServerError)
		return
	}
	if err := item.copyRange(a.cache, w, start, end); err != nil {
		return
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func parseRange(w http.ResponseWriter, r *http.Request, total int64) (start, end int64, status int, ok bool) {
	if total <= 0 {
		http.Error(w, "empty stream", http.StatusNotFound)
		return 0, 0, 0, false
	}
	start, end, status = 0, total-1, http.StatusOK
	spec := strings.TrimSpace(r.Header.Get("Range"))
	if spec == "" {
		return start, end, status, true
	}
	if !strings.HasPrefix(spec, "bytes=") || strings.Contains(spec, ",") {
		http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		return 0, 0, 0, false
	}
	first, last, _ := strings.Cut(strings.TrimPrefix(spec, "bytes="), "-")
	var firstErr, lastErr error
	if first != "" {
		start, firstErr = strconv.ParseInt(first, 10, 64)
	}
	if last != "" {
		end, lastErr = strconv.ParseInt(last, 10, 64)
	} else {
		end = total - 1
	}
	if first == "" {
		if last == "" || lastErr != nil {
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return 0, 0, 0, false
		}
		start = max(0, total-end)
		end = total - 1
	}
	if firstErr != nil || lastErr != nil || start < 0 || end < start || start >= total {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
		http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		return 0, 0, 0, false
	}
	if end >= total {
		end = total - 1
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	return start, end, http.StatusPartialContent, true
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func firstLANIPv4() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				if ip := ipnet.IP; ip.To4() != nil && !ip.IsLoopback() {
					return ip.String()
				}
			}
		}
	}
	return "127.0.0.1"
}
