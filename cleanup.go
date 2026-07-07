package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// handleCleanup performs a manual scan of the Lidarr music directory and deletes files
// that are not referenced in any .m3u playlist.
func handleCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	configMutex.RLock()
	playlistsPath := config.PlaylistsPath
	configMutex.RUnlock()

	referencedFiles := make(map[string]bool)
	filepath.WalkDir(playlistsPath, func(path string, d fs.DirEntry, err error) error {
		ext := strings.ToLower(filepath.Ext(path))
		if err != nil || d.IsDir() || (ext != ".m3u" && ext != ".m3u8") {
			return nil
		}
		playlistDir := filepath.Dir(path)
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		lines := strings.Split(string(content), "\n")
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}

			var fullAudioPath string
			if filepath.IsAbs(trimmed) {
				fullAudioPath = trimmed
			} else {
				fullAudioPath = filepath.Join(playlistDir, trimmed)
			}
			referencedFiles[filepath.Clean(fullAudioPath)] = true
		}
		return nil
	})

	var rootFolders []map[string]interface{}
	err := lidarrRequest("GET", "/api/v1/rootfolder", nil, &rootFolders)
	if err != nil || len(rootFolders) == 0 {
		http.Error(w, "Could not fetch Lidarr root folder", http.StatusInternalServerError)
		return
	}

	rootPath, ok := rootFolders[0]["path"].(string)
	if !ok || rootPath == "" {
		http.Error(w, "Invalid Lidarr root folder", http.StatusInternalServerError)
		return
	}

	deletedCount := 0
	filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".mp3" || ext == ".flac" || ext == ".m4a" || ext == ".ogg" {
			cleanPath := filepath.Clean(path)
			if !referencedFiles[cleanPath] {
				os.Remove(cleanPath)
				deletedCount++
			}
		}
		return nil
	})

	// Tell Lidarr to rescan the root folder to notice the deleted files
	lidarrRequest("POST", "/api/v1/command", map[string]interface{}{
		"name": "RescanFolders",
	}, nil)

	go startNavidromeScan()

	json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "deleted": deletedCount})
}
