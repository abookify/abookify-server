package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/pj/abookify/internal/db"
)

// SyncMessage represents a message in the sync protocol.
type SyncMessage struct {
	Type      string    `json:"type"`
	DeviceID  string    `json:"device_id,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	Data      any       `json:"data,omitempty"`
}

func (s *Server) handleRegisterDevice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Platform string `json:"platform"` // "ios", "android", "web"
		Token    string `json:"token"`    // pairing token from QR
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if !pairing.Consume(req.Token) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or expired pairing token"})
		return
	}

	// Generate device ID
	bytes := make([]byte, 16)
	rand.Read(bytes)
	deviceID := hex.EncodeToString(bytes)

	// Store the device
	s.store.SetSetting("device:"+deviceID+":name", req.Name)
	s.store.SetSetting("device:"+deviceID+":platform", req.Platform)
	s.store.SetSetting("device:"+deviceID+":registered", time.Now().UTC().Format(time.RFC3339))

	log.Printf("device registered: %s (%s, %s)", deviceID[:8], req.Name, req.Platform)

	writeJSON(w, http.StatusCreated, map[string]string{
		"device_id": deviceID,
	})
}

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.GetAllSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	type Device struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Platform   string `json:"platform"`
		Registered string `json:"registered"`
	}

	seen := map[string]bool{}
	var devices []Device

	for key, val := range settings {
		if len(key) > 7 && key[:7] == "device:" {
			// Extract device ID from "device:{id}:field"
			rest := key[7:]
			colonIdx := 0
			for i, c := range rest {
				if c == ':' {
					colonIdx = i
					break
				}
			}
			if colonIdx == 0 {
				continue
			}
			deviceID := rest[:colonIdx]
			field := rest[colonIdx+1:]

			if seen[deviceID] {
				continue
			}

			if field == "name" {
				seen[deviceID] = true
				devices = append(devices, Device{
					ID:         deviceID,
					Name:       val,
					Platform:   settings["device:"+deviceID+":platform"],
					Registered: settings["device:"+deviceID+":registered"],
				})
			}
		}
	}

	if devices == nil {
		devices = []Device{}
	}
	writeJSON(w, http.StatusOK, devices)
}

// handleSync handles a full sync request from a device.
// The device sends its local state, server responds with the merged state.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceID  string                 `json:"device_id"`
		Positions []db.PlaybackPosition  `json:"positions,omitempty"`
		Bookmarks []db.Bookmark          `json:"bookmarks,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}

	// Merge positions (last-write-wins based on timestamp)
	for _, pos := range req.Positions {
		existing, _ := s.store.GetPosition(pos.WorkID)
		if existing == nil || pos.PositionSecs > 0 {
			s.store.SavePosition(pos)
		}
	}

	// Merge bookmarks (additive — don't delete)
	for _, bm := range req.Bookmarks {
		s.store.CreateBookmark(bm)
	}

	// Return current server state
	works, _ := s.store.ListWorks()
	allPositions := []db.PlaybackPosition{}
	allBookmarks := []db.Bookmark{}

	for _, w := range works {
		if pos, err := s.store.GetPosition(w.ID); err == nil && pos != nil {
			allPositions = append(allPositions, *pos)
		}
		if bms, err := s.store.ListBookmarks(w.ID); err == nil {
			allBookmarks = append(allBookmarks, bms...)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"positions": allPositions,
		"bookmarks": allBookmarks,
	})
}
