package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// Network inputs may redirect or contain nested playlists. Keep those inputs
// on network protocols and bound stalled reads.
func remoteInputOptions(source string) []string {
	if !isRemoteTrack(source) {
		return nil
	}
	return []string{"-protocol_whitelist", "http,https,tcp,tls,crypto", "-rw_timeout", "15000000"}
}

type streamLog struct{ text []byte }

func (b *streamLog) Write(p []byte) (int, error) {
	n := len(p)
	if left := 4096 - len(b.text); left > 0 {
		b.text = append(b.text, p[:min(left, n)]...)
	}
	return n, nil
}

type liveWriter struct{ http.ResponseWriter }

func (w liveWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

func (a *App) serveLiveTrack(w http.ResponseWriter, r *http.Request, track Track) {
	// A live stream has neither a fixed length nor seekable byte positions.
	if spec := strings.TrimSpace(r.Header.Get("Range")); spec != "" && spec != "bytes=0-" {
		http.Error(w, "прямой эфир не поддерживает перемотку", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Accept-Ranges", "none")
	if r.Method == http.MethodHead {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stopShutdown := context.AfterFunc(a.cache.ctx, cancel)
	defer stopShutdown()
	args := []string{"-nostdin", "-loglevel", "error", "-hide_banner"}
	args = append(args, remoteInputOptions(track.source)...)
	args = append(args, "-i", track.source, "-vn", "-map", "0:a:0", "-map_metadata", "-1",
		"-c:a", "libmp3lame", "-b:a", "192k", "-ar", "44100", "-ac", "2",
		"-write_xing", "0", "-id3v2_version", "0", "-flush_packets", "1", "-f", "mp3", "pipe:1")
	cmd := exec.CommandContext(ctx, a.ffmpeg, args...)
	cmd.WaitDelay = 2 * time.Second
	stderr := &streamLog{}
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, "не удалось открыть аудиопоток", http.StatusBadGateway)
		return
	}
	if err := cmd.Start(); err != nil {
		_ = out.Close()
		http.Error(w, "не удалось запустить аудиопоток", http.StatusBadGateway)
		return
	}
	// Verify that audio actually arrives before committing a successful
	// response. Keep the first read small: waiting for a large ffmpeg buffer can
	// add several seconds of perceived live-stream startup latency.
	firstBuf := make([]byte, 1024)
	n, readErr := out.Read(firstBuf)
	var copyErr error
	if n > 0 {
		writer := liveWriter{w}
		_, copyErr = writer.Write(firstBuf[:n])
		if copyErr == nil && readErr == nil {
			buf := make([]byte, 32*1024)
			_, copyErr = io.CopyBuffer(writer, out, buf)
		}
	}
	if copyErr != nil || (readErr != nil && readErr != io.EOF) || n == 0 {
		cancel()
	}
	runErr := cmd.Wait()
	if n == 0 {
		http.Error(w, "не удалось получить аудио по ссылке: проверьте адрес и доступ к потоку", http.StatusBadGateway)
		return
	}
	if runErr != nil && r.Context().Err() == nil {
		log.Printf("remote live stream failed: %v: %s", runErr, stderr.text)
		// A broken upstream must not look like a clean end of a track.
		panic(http.ErrAbortHandler)
	}
}
