package main

import (
	"fmt"
	"log"
	"os/exec"
	"strings"

	"github.com/pj/abookify/internal/stt"
)

// hostHasGPU reports whether the MACHINE has an NVIDIA GPU, independent of
// whether the whisper container can see one. stt-cli runs on the host, so it
// can answer this — which is exactly the comparison nothing else was making.
//
// A var, not a func, so the guard itself is testable: the case worth testing is
// "host has a GPU, service does not", which is unreachable in a CI container
// where nvidia-smi is absent. Tests stub this; nothing else reassigns it.
var hostHasGPU = func() bool {
	out, err := exec.Command("nvidia-smi", "-L").Output()
	return err == nil && strings.Contains(string(out), "GPU")
}

// preflight reports what transcription will actually run on, and refuses to
// start a long run on CPU when the host has a GPU that simply is not attached.
//
// This has bitten twice in one day, both times invisibly. A plain
// `docker compose up whisper` reconciles from the base file alone, silently
// drops the GPU device reservation, and WHISPER_DEVICE=auto then honestly
// resolves to CPU. /health returns 200 and the model loads, so everything
// LOOKS fine — the only symptom is that 36 hours of audio would take 45 hours
// instead of 4. A 200 from /health does not mean the GPU is attached; the
// device field is the ground truth.
//
// Returns an error the caller should treat as fatal unless -allow-cpu was given.
func preflight(client *stt.Client, allowCPU bool, totalDurSecs float64) error {
	info, err := client.Info()
	if err != nil {
		// Don't block on an old service that has no /health detail; the run
		// itself will fail soon enough if the service is genuinely down.
		log.Printf("preflight: could not read STT service info (%v) — continuing", err)
		return nil
	}

	log.Printf("STT service: model=%s device=%s compute=%s",
		info.Model, info.Device, info.ComputeType)

	if info.Device == "cuda" {
		return nil
	}
	if !hostHasGPU() {
		log.Printf("preflight: running on %s (no GPU on this host)", info.Device)
		return nil
	}

	// Host has a GPU, the service does not. Quantify the cost so the choice is
	// informed rather than a shrug: CPU int8 runs ~0.8x realtime against ~8.6x
	// on this box's GPU.
	const cpuRate, gpuRate = 0.8, 8.6
	msg := fmt.Sprintf(
		"this host HAS an NVIDIA GPU but the STT service is running on %s/%s.\n"+
			"  This is almost always a whisper container started without the CUDA overlay:\n"+
			"      docker compose -f docker-compose.yml -f docker-compose.gpu.yml up -d whisper\n"+
			"  (or `make up`, which adds the overlay automatically when a GPU is present).",
		info.Device, info.ComputeType)
	if totalDurSecs > 0 {
		msg += fmt.Sprintf("\n  At CPU speed this run would take ~%.1f h instead of ~%.1f h.",
			totalDurSecs/3600/cpuRate, totalDurSecs/3600/gpuRate)
	}
	if allowCPU {
		log.Printf("WARNING: %s\n  Proceeding anyway (-allow-cpu).", msg)
		return nil
	}
	return fmt.Errorf("%s\n  Re-run with -allow-cpu to transcribe on CPU regardless", msg)
}
