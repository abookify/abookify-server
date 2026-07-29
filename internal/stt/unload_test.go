package stt

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestUnload_BothBackendShapes: the hermetic engine returns {"unloaded":bool}
// and the Docker whisper returns {"was_loaded":bool} for POST /unload. The Go
// client must read "freed" correctly from EITHER, or the idle-unload's
// observability silently breaks on one backend (it did, on Docker whisper).
func TestUnload_BothBackendShapes(t *testing.T) {
	cases := []struct {
		name, body string
		wantFreed  bool
	}{
		{"hermetic-engine freed", `{"unloaded":true}`, true},
		{"hermetic-engine noop", `{"unloaded":false}`, false},
		{"docker-whisper freed", `{"status":"ok","was_loaded":true,"model_loaded":false}`, true},
		{"docker-whisper noop", `{"status":"ok","was_loaded":false,"model_loaded":false}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/unload" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			freed, err := NewClient(srv.URL).Unload()
			if err != nil {
				t.Fatalf("unload: %v", err)
			}
			if freed != tc.wantFreed {
				t.Fatalf("freed=%v, want %v (body %s)", freed, tc.wantFreed, tc.body)
			}
		})
	}
}
