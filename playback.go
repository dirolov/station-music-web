package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The Station's BusyBox build does not include pkill. Scan ps and kill only
// maind processes launched by this app as "./maind --gst-launch ...".
// Terminating maind can leave dmix shared memory behind, even with SIGINT.
// A subsequent player then stays PREPARED because it inherits stale mixer
// state. Remove unused IPC pairs for the configured ALSA keys. Do not rely on
// cpid: ALSA records the first creator forever, while later maind processes
// attach as different PIDs/thread IDs.
const stopRemotePlaybackCommand = `pids=$(ps | while read pid user rest; do case "$rest" in './maind --gst-launch'*) echo "$pid";; esac; done)
for pid in $pids; do kill -INT "$pid" 2>/dev/null || true; done
for attempt in 1 2 3 4 5 6 7 8 9 10; do
  alive=
  for pid in $pids; do kill -0 "$pid" 2>/dev/null && alive="$alive $pid"; done
  [ -n "$alive" ] || break
  sleep 0.1
done
for pid in $alive; do kill -9 "$pid" 2>/dev/null || true; done
for attempt in 1 2 3 4 5 6 7 8 9 10; do
  alive=
  for pid in $pids; do kill -0 "$pid" 2>/dev/null && alive="$alive $pid"; done
  [ -n "$alive" ] || break
  sleep 0.1
done
[ -z "$alive" ] || exit 1
command -v ipcrm >/dev/null 2>&1 || exit 0
for key in $(awk '$1 == "ipc_key" && $2 ~ /^[0-9]+$/ {print $2}' /etc/asound.conf); do
  ids=$(awk -v key="$key" '
    NR>1 && ($1 == key || $1 == key+1) {
      if ($7 != 0) busy=1
      ids=ids " " $2
      if ($1 == key) control=1
    }
    END {if (control && !busy) print ids}
  ' /proc/sysvipc/shm)
  [ -n "$ids" ] || continue
  for id in $ids; do ipcrm -m "$id" 2>/dev/null || true; done
  for id in $(awk -v key="$key" 'NR>1 && $1 == key {print $2}' /proc/sysvipc/sem); do
    ipcrm -s "$id" 2>/dev/null || true
  done
done`

// The command deliberately uses "[m]aind" so its own shell line is not matched
// by grep. BusyBox ps does not show a stable parent/child relationship, and
// the app intentionally keeps at most one gst-launch process.
const stationVolumeCommand = `limits=$(amixer get Master 2>/dev/null | grep 'Limits: Playback' | head -n 1)
channel=$(amixer get Master 2>/dev/null | grep -E '(Front Left|Mono): Playback' | head -n 1)
printf 'volume_limits=%s\nvolume_channel=%s\n' "$limits" "$channel"`

const playbackHealthCommand = stationVolumeCommand + `
pid=$(ps | grep "[m]aind --gst-launch" | awk '{print $1}' | head -n 1)
if [ -z "$pid" ]; then printf 'process=absent\n'; exit 0; fi
printf 'process=present\n'
pcm=/proc/asound/card0/pcm0p/sub0/status
state=$(sed -n 's/^state:[[:space:]]*//p' "$pcm" 2>/dev/null | head -n 1 | tr -d '\r')
ptr=$(grep '^hw_ptr' "$pcm" 2>/dev/null | head -n 1 | awk '{print $NF}')
[ -n "$state" ] || state=closed
[ -n "$ptr" ] || ptr=0
printf 'pcm_state=%s\npcm_hw_ptr=%s\n' "$state" "$ptr"`

type PlaybackHealth struct {
	CheckedAt      time.Time
	RemoteProcess  bool
	AudioState     string
	AudioHwPtr     int64
	AudioAdvancing bool
	StationError   string
	Volume         *VolumeStatus
}

type VolumeStatus struct {
	Percent int    `json:"percent"`
	Raw     int    `json:"raw"`
	Max     int    `json:"max"`
	Error   string `json:"error,omitempty"`
}

var (
	volumeLimitsPattern  = regexp.MustCompile(`^[[:space:]]*Limits:[[:space:]]+Playback[[:space:]]+([0-9]+)[[:space:]]+-[[:space:]]+([0-9]+)$`)
	volumeChannelPattern = regexp.MustCompile(`^[[:space:]]*(?:Front Left|Mono):[[:space:]]+Playback[[:space:]]+([0-9]+)[[:space:]]+\[([0-9]+)%\]`)
)

func (a *App) startPlaybackAt(index int) error {
	a.playMu.Lock()
	defer a.playMu.Unlock()

	return a.startPlaybackAtLocked(index)
}

// Caller holds playMu so manual controls and automatic transitions stay ordered.
func (a *App) startPlaybackAtLocked(index int) error {
	a.stopPlaybackLocked(true)
	cfg := a.config()
	if err := validateStationTarget(cfg); err != nil {
		return err
	}
	a.queueMu.Lock()
	if a.queue == nil || len(a.queue.Tracks) == 0 {
		a.queueMu.Unlock()
		return errors.New("queue is empty")
	}
	if index < 0 || index >= len(a.queue.Tracks) {
		a.queueMu.Unlock()
		return fmt.Errorf("track index %d is out of range", index)
	}
	queueID := a.queue.ID
	a.queueMu.Unlock()

	streamURL := a.streamURL(cfg, queueID, index)
	pipeline := "souphttpsrc location=" + streamURL + " ! decodebin ! audioconvert ! audioresample ! alsasink"
	remoteCommand := strings.Join([]string{
		"cd /system/vendor/quasar",
		"LD_LIBRARY_PATH=/system/vendor/quasar:/system/vendor/quasar/libs",
		"./maind --gst-launch " + shellQuote(pipeline),
	}, " && ")

	playCtx, cancel := context.WithCancel(context.Background())
	cmd := stationCommandContext(playCtx, cfg, remoteCommand)
	stderr := &bytes.Buffer{}
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start station command: %w", err)
	}

	active := &Active{
		QueueID:    queueID,
		TrackIndex: index,
		StartedAt:  time.Now().UTC(),
		Cancel:     cancel,
		Cmd:        cmd,
		Done:       make(chan struct{}),
	}
	a.queueMu.Lock()
	a.active = active
	a.queueMu.Unlock()

	go func() {
		runErr := cmd.Wait()
		if runErr != nil {
			runErr = fmt.Errorf("%w: %s", runErr, bytes.TrimSpace(stderr.Bytes()))
			log.Printf("playback track %d exited with error: %v", index, runErr)
		} else {
			log.Printf("playback track %d exited cleanly", index)
		}
		a.queueMu.Lock()
		if a.active == active && !active.Stopped {
			active.Err = runErr
		}
		close(active.Done)
		a.queueMu.Unlock()
		a.advancePlayback(active, runErr)
	}()
	return nil
}

func (a *App) advancePlayback(active *Active, runErr error) {
	a.playMu.Lock()
	defer a.playMu.Unlock()

	// Recheck after obtaining playMu: stop, switching tracks or replacing the
	// queue may have happened while the completed process was waiting for it.
	a.queueMu.Lock()
	advance := a.active == active && shouldAutoAdvance(active, runErr, a.queue)
	a.queueMu.Unlock()
	if advance {
		if err := a.startPlaybackAtLocked(active.TrackIndex + 1); err != nil {
			log.Printf("auto-advance to track %d: %v", active.TrackIndex+1, err)
		}
	}
}

func shouldAutoAdvance(active *Active, runErr error, queue *Queue) bool {
	if active == nil || runErr != nil || active.Stopped {
		return false
	}
	if queue == nil || queue.ID != active.QueueID {
		return false
	}
	return active.TrackIndex+1 < len(queue.Tracks)
}

func (a *App) stopPlayback(wait bool) {
	a.playMu.Lock()
	defer a.playMu.Unlock()
	a.stopPlaybackLocked(wait)
}

func (a *App) stopPlaybackLocked(wait bool) {
	a.queueMu.Lock()
	active := a.active
	if active != nil {
		active.Stopped = true
		if a.active == active {
			a.active = nil
		}
	}
	a.queueMu.Unlock()
	// A local adb/ssh client can exit before remote maind. Always clean up a
	// possible orphan, even when active is already nil.
	if cfg := a.config(); validateStationTarget(cfg) == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = runStationCommand(ctx, cfg, stopRemotePlaybackCommand)
		cancel()
	}
	if active != nil {
		active.Cancel()
		if wait {
			select {
			case <-active.Done:
			case <-time.After(4 * time.Second):
			}
		}
	}
}

func validateStationTarget(cfg Config) error {
	switch cfg.StationMode {
	case "adb":
		if strings.TrimSpace(cfg.StationSerial) == "" {
			return errors.New("ADB serial is empty; configure IP:5555 in Settings")
		}
	case "ssh":
		if strings.TrimSpace(cfg.StationHost) == "" {
			return errors.New("SSH host is empty; configure the Station IP in Settings")
		}
	default:
		return fmt.Errorf("unsupported station mode %q", cfg.StationMode)
	}
	return nil
}

func runStationCommand(ctx context.Context, cfg Config, command string) error {
	_, err := runStationCommandOutput(ctx, cfg, command)
	return err
}

func runStationCommandOutput(ctx context.Context, cfg Config, command string) (string, error) {
	cmd := stationCommandContext(ctx, cfg, command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.String(), nil
}

func (a *App) probePlaybackHealth(previous PlaybackHealth) PlaybackHealth {
	health := PlaybackHealth{CheckedAt: time.Now().UTC(), AudioState: "unknown", Volume: &VolumeStatus{}}
	cfg := a.config()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := runStationCommandOutput(ctx, cfg, playbackHealthCommand)
	if err != nil {
		health.StationError = fmt.Sprintf("не удалось проверить колонку: %v", err)
		health.Volume.Error = health.StationError
		return health
	}
	parsed, ok := parsePlaybackHealthOutput(out, health.CheckedAt)
	if !ok {
		health.StationError = "не удалось разобрать ответ проверки звука"
		return health
	}
	return playbackHealthWithProgress(parsed, previous)
}

func playbackHealthWithProgress(health, previous PlaybackHealth) PlaybackHealth {
	// A live PCM must move its hardware pointer before reporting playback as
	// successful. Compare every later sample, including subsecond polls; a
	// minimum interval would never elapse when multiple clients poll frequently.
	health.AudioAdvancing = health.RemoteProcess && previous.RemoteProcess &&
		health.AudioState == "RUNNING" && previous.AudioState == "RUNNING" &&
		health.CheckedAt.After(previous.CheckedAt) &&
		health.AudioHwPtr > previous.AudioHwPtr
	return health
}

func parsePlaybackHealthOutput(out string, checkedAt time.Time) (PlaybackHealth, bool) {
	values := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	if process, ok := values["process"]; !ok || (process != "present" && process != "absent") {
		return PlaybackHealth{}, false
	}
	health := PlaybackHealth{CheckedAt: checkedAt, RemoteProcess: values["process"] == "present"}
	volume := parseVolumeStatusValues(values)
	health.Volume = &volume
	if !health.RemoteProcess {
		health.AudioState = "closed"
		return health, true
	}
	state, ok := values["pcm_state"]
	if !ok || state == "" {
		return PlaybackHealth{}, false
	}
	ptrText, ok := values["pcm_hw_ptr"]
	if !ok {
		return PlaybackHealth{}, false
	}
	ptr, err := strconv.ParseInt(ptrText, 10, 64)
	if err != nil || ptr < 0 {
		return PlaybackHealth{}, false
	}
	health.AudioState = state
	health.AudioHwPtr = ptr
	return health, true
}

func (a *App) volumeStatus() VolumeStatus {
	a.volumeMu.Lock()
	defer a.volumeMu.Unlock()

	cfg := a.config()
	if err := validateStationTarget(cfg); err != nil {
		return VolumeStatus{Error: err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := runStationCommandOutput(ctx, cfg, stationVolumeCommand)
	if err != nil {
		return VolumeStatus{Error: fmt.Sprintf("не удалось проверить громкость: %v", err)}
	}
	volume := parseVolumeStatusOutput(out)
	a.lastVolume = &volume
	return volume
}

func setStationVolumeCommand(percent int) string {
	return fmt.Sprintf("/system/vendor/quasar/set_volume.sh %d >/dev/null 2>&1 && %s", percent, stationVolumeCommand)
}

func parseVolumeStatusOutput(out string) VolumeStatus {
	values := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return parseVolumeStatusValues(values)
}

func parseVolumeStatusValues(values map[string]string) VolumeStatus {
	raw, max, percent, ok := parseVolumeLines(values["volume_limits"], values["volume_channel"])
	if !ok {
		return VolumeStatus{Error: "не удалось разобрать ответ amixer"}
	}
	return VolumeStatus{Percent: percent, Raw: raw, Max: max}
}

func parseVolumeLines(limits, channel string) (raw, max, percent int, ok bool) {
	limitsMatch := volumeLimitsPattern.FindStringSubmatch(strings.TrimSpace(limits))
	channelMatch := volumeChannelPattern.FindStringSubmatch(strings.TrimSpace(channel))
	if limitsMatch == nil || channelMatch == nil {
		return 0, 0, 0, false
	}
	raw, rawErr := strconv.Atoi(channelMatch[1])
	percent, percentErr := strconv.Atoi(channelMatch[2])
	max, maxErr := strconv.Atoi(limitsMatch[2])
	if rawErr != nil || percentErr != nil || maxErr != nil || max <= 0 || raw < 0 || raw > max || percent < 0 || percent > 100 {
		return 0, 0, 0, false
	}
	return raw, max, percent, true
}

func activePlaybackState(active *Active, health PlaybackHealth, now time.Time) (playing bool, state string, finished bool) {
	localRunning := activeLocalRunning(active)
	finished = !localRunning && !health.RemoteProcess

	if health.StationError != "" {
		return false, "unknown", finished
	}
	if !health.RemoteProcess {
		if localRunning && now.Sub(active.StartedAt) < 3*time.Second {
			return false, "starting", false
		}
		if !localRunning {
			return false, "finished", true
		}
		return false, "stopping", false
	}
	if health.AudioState == "RUNNING" && health.AudioAdvancing {
		return true, "playing", false
	}
	if now.Sub(active.StartedAt) < 3*time.Second {
		return false, "starting", false
	}
	return false, "no_sound", false
}

func activeLocalRunning(active *Active) bool {
	select {
	case <-active.Done:
		return false
	default:
		return true
	}
}

func stationCommandContext(ctx context.Context, cfg Config, command string) *exec.Cmd {
	if cfg.StationMode == "ssh" {
		destination := cfg.StationUser + "@" + cfg.StationHost
		return exec.CommandContext(ctx, findTool("ssh"),
			"-p", strconv.Itoa(cfg.StationPort),
			"-o", "BatchMode=yes",
			"-o", "ConnectTimeout=5",
			"-o", "StrictHostKeyChecking=accept-new",
			destination, command,
		)
	}
	return exec.CommandContext(ctx, findTool("adb"), "-s", cfg.StationSerial, "shell", command)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (a *App) streamURL(cfg Config, queueID string, index int) string {
	return fmt.Sprintf("%s/music/q/%s/%d.mp3?token=%s", resolvePublicBase(cfg), queueID, index, a.token)
}

func resolvePublicBase(cfg Config) string {
	if base := strings.TrimRight(strings.TrimSpace(cfg.PublicBase), "/"); base != "" {
		return base
	}
	host, port, err := net.SplitHostPort(cfg.Address)
	if err != nil {
		host, port = "", "8080"
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = firstLANIPv4()
	}
	return "http://" + net.JoinHostPort(host, port)
}

func estimateTrackIndex(q Queue, active *Active, now time.Time) int {
	if active == nil || len(q.Tracks) == 0 {
		return 0
	}
	_ = now
	return active.TrackIndex
}
