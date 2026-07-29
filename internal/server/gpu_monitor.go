package server

import (
	"sync/atomic"
	"time"

	"github.com/pj/abookify/internal/applog"
	"github.com/pj/abookify/internal/stt"
)

// whisperMonitorInterval is how often the device monitor polls whisper /health.
// 30s is cheap (one tiny GET) yet catches a downgrade within half a minute —
// fast enough to flag a run that just dropped to ~10× slower CPU transcription.
const whisperMonitorInterval = 30 * time.Second

// whisperMonStarted guards against starting the monitor twice (SetReady(true)
// can fire more than once over a process's life).
var whisperMonStarted atomic.Bool

// startWhisperDeviceMonitor launches a background goroutine that watches the
// LOCAL whisper service's compute device and raises a LOUD alarm if it
// downgrades cuda→cpu while the server is running.
//
// WHY this exists, and why a startup check isn't enough: the COMPOSE_FILE guard
// (`make gpu-enable`) PREVENTS a `docker compose` recreate from landing whisper
// on CPU — that covers every historical incident, all of which were bare-compose
// recreates. But it can't PREVENT a downgrade that arrives by a non-compose path
// (WHISPER_DEVICE=auto degrading on a driver blip, an OOM CPU fallback, a manual
// container action), and in every one of those the device changes *mid-run*,
// after any boot-time check has already passed — so /health keeps returning 200
// with device=cpu and nothing notices. This monitor is the DETECTION half: it
// polls continuously and alarms on the transition.
//
// DETECTION ONLY. It never touches the transcription job (no pause/abort): a
// false abort of a multi-hour run is worse than a loud log. Escalating to job
// control is a separate, transcription-owned decision (sentinel option C).
func (s *Server) startWhisperDeviceMonitor() {
	if s.STTURL == "" {
		return // no local engine wired (API-key-only install) — nothing to watch
	}
	if !whisperMonStarted.CompareAndSwap(false, true) {
		return // already running
	}
	go s.whisperDeviceMonitorLoop()
}

// deviceAlarm is the action a single device reading warrants.
type deviceAlarm int

const (
	alarmNone       deviceAlarm = iota
	alarmStartedCPU             // first-ever read is cpu (info: slow, not a regression)
	alarmDowngrade             // cuda→cpu after a known-good state (LOUD error)
	alarmRestored              // cpu→cuda recovery (info)
)

// deviceTransition decides, from the last-known baseline and a freshly read
// device, the new baseline and which alarm (if any) to raise. Pure +
// table-tested — the polling loop is the only untested shell around it. An
// empty `dev` (undecodable read) is treated as no-signal: baseline unchanged.
func deviceTransition(baseline, dev string) (newBaseline string, alarm deviceAlarm) {
	switch {
	case dev == "":
		return baseline, alarmNone
	case baseline == "":
		if dev == "cpu" {
			return dev, alarmStartedCPU
		}
		return dev, alarmNone
	case baseline == "cuda" && dev == "cpu":
		return dev, alarmDowngrade // adopt cpu so we don't re-alarm every tick
	case baseline == "cpu" && dev == "cuda":
		return dev, alarmRestored
	default:
		return baseline, alarmNone
	}
}

func (s *Server) whisperDeviceMonitorLoop() {
	client := stt.NewClient(s.STTURL)
	baseline := "" // last KNOWN device; "" until the first successful read
	ticker := time.NewTicker(whisperMonitorInterval)
	defer ticker.Stop()

	for range ticker.C {
		// Only watch when the LOCAL whisper is the active STT provider. A cloud
		// (BYOK) install has no local GPU to lose, and its /health is irrelevant.
		if prov, _ := s.store.GetSetting("stt_provider"); prov == "openai" {
			continue
		}

		info, err := client.Info()
		if err != nil {
			// Unreachable = whisper down/restarting (e.g. a legitimate recreate
			// in flight). A different condition — don't alarm and don't move the
			// baseline; the next successful read re-checks against it, so a
			// healthy recreate that comes back on cuda produces no false alarm.
			continue
		}

		var alarm deviceAlarm
		baseline, alarm = deviceTransition(baseline, info.Device)
		switch alarm {
		case alarmStartedCPU:
			applog.Info("stt-gpu", "whisper is on CPU (device=cpu) — STT will be slow; if this host has a GPU, restore it with `make whisper`")
		case alarmDowngrade:
			applog.Log(applog.LevelError, "stt-gpu", "", 0,
				"whisper DOWNGRADED to CPU mid-flight (device cuda→cpu) — STT is now ~10× slower. A driver/OOM fault or a bare `docker compose up` likely stripped the GPU. Restore with `make whisper`, then verify device=cuda.",
				map[string]any{"from": "cuda", "to": "cpu"})
			s.Events.Broadcast(Event{Type: "stt_device_downgrade", Data: map[string]any{"from": "cuda", "to": "cpu"}})
		case alarmRestored:
			applog.Info("stt-gpu", "whisper is back on GPU (device cpu→cuda)")
			s.Events.Broadcast(Event{Type: "stt_device_restored", Data: map[string]any{"from": "cpu", "to": "cuda"}})
		}
	}
}
