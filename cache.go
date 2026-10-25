package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type Cache struct {
	ctx    context.Context
	cancel context.CancelFunc
	dir    string
	ffmpeg string

	mu     sync.Mutex
	tracks map[string]*CachedTrack
	closed bool

	silenceMu sync.Mutex
	silence   []byte
}

type CachedTrack struct {
	key   string
	input string
	path  string
	file  *os.File

	mu   sync.Mutex
	cond *sync.Cond
	size int64
	done bool
	err  error
}

func NewCache(parent context.Context, dir string) *Cache {
	ctx, cancel := context.WithCancel(parent)
	return &Cache{
		ctx:    ctx,
		cancel: cancel,
		dir:    dir,
		ffmpeg: findTool("ffmpeg"),
		tracks: make(map[string]*CachedTrack),
	}
}

func (c *Cache) Get(track Track) (*CachedTrack, error) {
	if track.Live {
		return nil, fmt.Errorf("live streams must not be written to the disk cache")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("cache is closed")
	}
	if item, ok := c.tracks[track.ID]; ok {
		return item, nil
	}
	key := filepath.Join(c.dir, track.ID+".mp3")
	part := key + ".part"
	f, err := os.Open(key)
	if err == nil {
		info, statErr := f.Stat()
		if statErr == nil {
			item := &CachedTrack{key: key, input: track.source, path: key, file: f}
			item.cond = sync.NewCond(&item.mu)
			item.size = info.Size()
			item.done = true
			c.tracks[track.ID] = item
			return item, nil
		}
		f.Close()
	}

	writer, err := os.Create(part)
	if err != nil {
		return nil, err
	}
	reader, err := os.Open(part)
	if err != nil {
		_ = writer.Close()
		return nil, err
	}
	item := &CachedTrack{key: key, input: track.source, path: part, file: reader}
	item.cond = sync.NewCond(&item.mu)

	args := []string{
		"-nostdin", "-loglevel", "error", "-hide_banner", "-y",
	}
	args = append(args, remoteInputOptions(item.input)...)
	args = append(args, "-i", item.input,
		"-vn", "-map_metadata", "-1",
		"-c:a", "libmp3lame", "-b:a", "192k", "-ar", "44100", "-ac", "2",
		"-write_xing", "0", "-id3v2_version", "0", "-f", "mp3", part,
	)
	cmd := exec.CommandContext(c.ctx, c.ffmpeg, args...)
	stderr := &bytes.Buffer{}
	cmd.Stdout = nil
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		_ = os.Remove(part)
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	c.tracks[track.ID] = item
	monitorDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-monitorDone:
				return
			case <-ticker.C:
				item.mu.Lock()
				if item.done {
					item.mu.Unlock()
					return
				}
				if info, err := item.file.Stat(); err == nil && info.Size() > item.size {
					item.size = info.Size()
					item.cond.Broadcast()
				}
				done := item.done
				item.mu.Unlock()
				if done {
					return
				}
			}
		}
	}()

	go func() {
		runErr := cmd.Wait()
		close(monitorDone)
		item.mu.Lock()
		defer item.mu.Unlock()
		_ = writer.Close()
		if info, err := item.file.Stat(); err == nil {
			item.size = info.Size()
		}
		if runErr == nil {
			_ = item.file.Sync()
			if err := os.Rename(part, key); err == nil {
				item.path = key
			}
			// Even if renaming fails, the open .part file remains valid.
			item.err = nil
		} else {
			item.err = fmt.Errorf("ffmpeg: %w: %s", runErr, bytes.TrimSpace(stderr.Bytes()))
		}
		item.done = true
		item.cond.Broadcast()
	}()
	return item, nil
}

func (c *Cache) Close() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.cancel()
		for _, item := range c.tracks {
			item.mu.Lock()
			_ = item.file.Close()
			item.mu.Unlock()
		}
	}
	c.mu.Unlock()
}

func (c *Cache) getSilence() []byte {
	c.silenceMu.Lock()
	defer c.silenceMu.Unlock()
	if len(c.silence) > 0 {
		return c.silence
	}
	cmd := exec.CommandContext(c.ctx, c.ffmpeg,
		"-nostdin", "-loglevel", "error", "-f", "lavfi",
		"-i", "anullsrc=r=44100:cl=stereo", "-t", "1",
		"-c:a", "libmp3lame", "-b:a", "192k", "-ar", "44100", "-ac", "2",
		"-write_xing", "0", "-id3v2_version", "0", "-f", "mp3", "-",
	)
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return make([]byte, 4096)
	}
	c.silence = out
	return c.silence
}

// copyRange writes [from,to] of a fixed-size track slot. The MP3 file can still
// be growing; remaining slot bytes are filled with MP3 silence.
func (t *CachedTrack) copyRange(c *Cache, w writeFlusher, from, to int64) error {
	if from < 0 || to < from {
		return fmt.Errorf("invalid copy range %d-%d", from, to)
	}
	silence := c.getSilence()
	buf := make([]byte, 64*1024)
	pos := from
	for pos <= to {
		t.mu.Lock()
		for !t.done && t.size <= pos {
			t.cond.Wait()
		}
		size, done, err := t.size, t.done, t.err
		t.mu.Unlock()
		if err != nil {
			return err
		}
		want := to - pos + 1
		switch {
		case pos < size:
			n, readErr := t.file.ReadAt(buf[:min(int64(len(buf)), want, size-pos)], pos)
			if n == 0 && readErr == nil {
				continue
			}
			if n > 0 {
				if _, writeErr := w.Write(buf[:n]); writeErr != nil {
					return writeErr
				}
				pos += int64(n)
			} else if readErr != nil && !errors.Is(readErr, io.EOF) {
				return readErr
			}
		case done:
			off := int((pos - size) % int64(len(silence)))
			n := min(int64(len(silence)-off), want)
			if _, writeErr := w.Write(silence[off : off+int(n)]); writeErr != nil {
				return writeErr
			}
			pos += n
		}
	}
	return nil
}

type writeFlusher interface {
	Write(p []byte) (int, error)
}
