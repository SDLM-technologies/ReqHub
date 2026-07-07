package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DeletedTrack represents a unified structure for capturing both file-level metadata 
// and Lidarr/MusicBrainz metadata when a track or album is deleted via the disk cleaner.
type DeletedTrack struct {
	Timestamp int64  `json:"timestamp,omitempty"` // Legacy: Unix timestamp of deletion
	TrackPath string `json:"trackPath,omitempty"` // Legacy: Absolute file path of the audio
	Playlist  string `json:"playlist,omitempty"`  // Legacy: The playlist it was removed from
	
	ArtistId         int    `json:"artistId,omitempty"`         // Lidarr specific artist ID
	ForeignReleaseId string `json:"foreignReleaseId,omitempty"` // MusicBrainz Release ID
	AlbumTitle       string `json:"albumTitle,omitempty"`       // Full album title
	DeletedAt        string `json:"deletedAt,omitempty"`        // ISO-8601 timestamp of deletion
}

var deletedHistoryMutex sync.Mutex

// logDeletedTrack is a legacy helper to log a single track deletion based on its file path.
// It opens the JSON history ledger, prepends the new track, and rewrites the file safely.
func logDeletedTrack(trackPath, playlist string) {
	deletedHistoryMutex.Lock()
	defer deletedHistoryMutex.Unlock()

	historyFile := filepath.Join("data", "deleted_history.json")
	var history []DeletedTrack

	if content, err := os.ReadFile(historyFile); err == nil {
		json.Unmarshal(content, &history)
	}

	history = append(history, DeletedTrack{
		Timestamp: time.Now().Unix(),
		TrackPath: trackPath,
		Playlist:  playlist,
	})

	if b, err := json.MarshalIndent(history, "", "  "); err == nil {
		os.MkdirAll("data", 0755)
		os.WriteFile(historyFile, b, 0644)
	}
}

// handleGetHistory handles GET requests to retrieve the full ledger of deleted tracks.
// This is consumed by the History UI to display past deletions.
func handleGetHistory(w http.ResponseWriter, r *http.Request) {
	historyFile := filepath.Join("data", "deleted_history.json")
	var history []DeletedTrack
	if content, err := os.ReadFile(historyFile); err == nil {
		json.Unmarshal(content, &history)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(history)
}

// handleRestoreHistory handles POST requests from the History UI.
// It parses the requested restore type (single, batch, or all), filters the ledger,
// simulates the restoration of matching items (e.g., by sending requests to Lidarr),
// and finally updates the ledger by stripping out the restored items.
func handleRestoreHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var req struct {
		Type string `json:"type"` // "single", "batch", "all"
		ForeignId string `json:"foreignId,omitempty"`
		Date string `json:"date,omitempty"`
	}
	
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	historyFile := filepath.Join("data", "deleted_history.json")
	var history []DeletedTrack
	if content, err := os.ReadFile(historyFile); err == nil {
		json.Unmarshal(content, &history)
	}

	var remaining []DeletedTrack
	var restoredCount int

	for _, item := range history {
		match := false
		if req.Type == "all" {
			match = true
		} else if req.Type == "single" && item.ForeignReleaseId == req.ForeignId {
			match = true
		} else if req.Type == "batch" && item.DeletedAt == req.Date {
			match = true
		}

		if match {
			restoredCount++
		} else {
			remaining = append(remaining, item)
		}
	}

	if b, err := json.MarshalIndent(remaining, "", "  "); err == nil {
		os.WriteFile(historyFile, b, 0644)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "restored": restoredCount})
}
