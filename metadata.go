package main

import (
	"errors"
	"fmt"
	"os"
	"sync"
)

const metadataWorkers = 4
const metadataCacheLimit = 4096

type probeResult struct {
	duration int
	tags     map[string]string
	err      error
	done     chan struct{}
}

type metadataCache struct {
	once    sync.Once
	mu      sync.Mutex
	entries map[string]*probeResult
	slots   chan struct{}
}

// Cache raw metadata, not Track: playlist fallback tags belong to each entry.
// Local cache keys change when a file is modified. Remote sources are not cached.
// In-flight entries also coalesce simultaneous requests for the same file.
func (a *App) cachedProbe(source string) (int, map[string]string, error) {
	c := &a.metadata
	c.once.Do(func() {
		c.entries = make(map[string]*probeResult)
		c.slots = make(chan struct{}, metadataWorkers)
	})
	key := ""
	if info, err := os.Stat(source); err == nil {
		key = fmt.Sprintf("%s:%d:%d", source, info.ModTime().UnixNano(), info.Size())
	}
	c.mu.Lock()
	if result := c.entries[key]; key != "" && result != nil {
		c.mu.Unlock()
		<-result.done
		return result.duration, result.tags, result.err
	}
	result := &probeResult{done: make(chan struct{})}
	if key != "" {
		if len(c.entries) >= metadataCacheLimit {
			// Evict completed entries only; retain in-flight deduplication.
			for k, v := range c.entries {
				select {
				case <-v.done:
					delete(c.entries, k)
				default:
				}
			}
		}
		c.entries[key] = result
	}
	c.mu.Unlock()

	c.slots <- struct{}{}
	result.duration, result.tags, result.err = a.probeSource(source)
	<-c.slots
	c.mu.Lock()
	if result.err != nil {
		delete(c.entries, key) // Transient failures must be retried.
	}
	close(result.done)
	c.mu.Unlock()
	return result.duration, result.tags, result.err
}

type trackCandidate struct {
	source, display, title, artist, album string
	duration                              int
	rejected                              bool
}

func (a *App) expandPaths(paths []string, recursive bool) ([]Track, []string, error) {
	type expansion struct {
		tracks  []Track
		skipped []string
		err     error
	}
	results := make([]expansion, len(paths))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < min(metadataWorkers, len(paths)); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i].tracks, results[i].skipped, results[i].err = a.expandPath(paths[i], recursive)
			}
		}()
	}
	for i := range paths {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	var tracks []Track
	var skipped []string
	for _, result := range results {
		if result.err != nil {
			return nil, nil, result.err
		}
		tracks = append(tracks, result.tracks...)
		skipped = append(skipped, result.skipped...)
	}
	return tracks, skipped, nil
}

// Probe a bounded number of files concurrently and collect results in input
// order, including skipped entries. The shared limiter also bounds other requests.
func (a *App) resolveCandidates(root string, candidates []trackCandidate) ([]Track, []string) {
	results := make([]Track, len(candidates))
	failures := make([]error, len(candidates))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < min(metadataWorkers, len(candidates)); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				c := candidates[i]
				if c.rejected {
					failures[i] = errors.New("absolute playlist path is not allowed")
					continue
				}
				var track Track
				var err error
				if isRemoteTrack(c.source) {
					track, err = a.trackFromURL(c.source, c.title, c.duration)
				} else {
					track, err = a.trackFromAbs(root, c.source, c.title, c.duration)
				}
				if track.Artist == "" {
					track.Artist = c.artist
				}
				if track.Album == "" {
					track.Album = c.album
				}
				results[i], failures[i] = track, err
			}
		}()
	}
	for i := range candidates {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	var tracks []Track
	var skipped []string
	for i, track := range results {
		if failures[i] != nil {
			skipped = append(skipped, candidates[i].display)
		} else {
			tracks = append(tracks, track)
		}
	}
	return tracks, skipped
}
