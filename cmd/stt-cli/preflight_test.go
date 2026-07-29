package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/stt"
)

func fakeSTT(t *testing.T, device, compute string) *stt.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "model": "large-v3",
			"device": device, "compute_type": compute,
			"gpu_available": device == "cuda",
		})
	}))
	t.Cleanup(srv.Close)
	return stt.NewClient(srv.URL)
}

// A service on cuda is always fine.
func TestPreflightAcceptsCUDA(t *testing.T) {
	if err := preflight(fakeSTT(t, "cuda", "float16"), false, 3600); err != nil {
		t.Errorf("cuda should pass preflight: %v", err)
	}
}

// The case that has cost hours twice: the host has a GPU, the container does
// not, and /health still answers 200 so nothing looks wrong. This must be
// fatal, and the message must say how to fix it — the whole point is that the
// operator cannot see the problem otherwise.
func TestPreflightRejectsCPUWhenHostHasGPU(t *testing.T) {
	orig := hostHasGPU
	hostHasGPU = func() bool { return true }
	defer func() { hostHasGPU = orig }()

	err := preflight(fakeSTT(t, "cpu", "int8"), false, 36*3600)
	if err == nil {
		t.Fatal("CPU service on a GPU host must fail preflight")
	}
	for _, want := range []string{"docker-compose.gpu.yml", "allow-cpu"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should tell the operator how to fix it (missing %q): %v", want, err)
		}
	}
	// The cost is what makes the choice informed rather than a shrug.
	if !strings.Contains(err.Error(), "instead of") {
		t.Errorf("error should quantify the cost: %v", err)
	}
}

// -allow-cpu is an explicit, deliberate override — it must not be blocked.
func TestPreflightAllowCPUOverride(t *testing.T) {
	orig := hostHasGPU
	hostHasGPU = func() bool { return true }
	defer func() { hostHasGPU = orig }()

	if err := preflight(fakeSTT(t, "cpu", "int8"), true, 3600); err != nil {
		t.Errorf("-allow-cpu must proceed: %v", err)
	}
}

// No GPU on the host at all: CPU is simply the correct answer, not a warning.
func TestPreflightQuietWhenHostHasNoGPU(t *testing.T) {
	orig := hostHasGPU
	hostHasGPU = func() bool { return false }
	defer func() { hostHasGPU = orig }()

	if err := preflight(fakeSTT(t, "cpu", "int8"), false, 3600); err != nil {
		t.Errorf("CPU on a CPU-only host must pass: %v", err)
	}
}

// A service too old to report a device must not block a run.
func TestPreflightTolerantOfUnreachableInfo(t *testing.T) {
	if err := preflight(stt.NewClient("http://127.0.0.1:1"), false, 3600); err != nil {
		t.Errorf("unreadable /health should warn, not block: %v", err)
	}
}
