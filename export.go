package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
)

type ExportData struct {
	Config    Config              `json:"config"`
	Playlists map[string][]string `json:"playlists"`
	History   []DeletedTrack      `json:"history"`
}

func handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	configMutex.RLock()
	currentConfig := config
	configMutex.RUnlock()

	data := ExportData{
		Config:    currentConfig,
		Playlists: make(map[string][]string),
	}

	// Read all playlists
	filepath.WalkDir(currentConfig.PlaylistsPath, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if content, err := os.ReadFile(path); err == nil {
				data.Playlists[filepath.Base(path)] = []string{string(content)}
			}
		}
		return nil
	})

	// Read history
	historyFile := filepath.Join("data", "deleted_history.json")
	if content, err := os.ReadFile(historyFile); err == nil {
		json.Unmarshal(content, &data.History)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=\"reqhub_export.json\"")
	json.NewEncoder(w).Encode(data)
}

func handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var data ExportData
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	configMutex.Lock()
	config = data.Config
	configMutex.Unlock()
	b, _ := json.MarshalIndent(data.Config, "", "  ")
	os.MkdirAll("data", 0755)
	os.WriteFile("data/config.json", b, 0644)

	os.MkdirAll(config.PlaylistsPath, 0755)
	for name, lines := range data.Playlists {
		if len(lines) > 0 {
			os.WriteFile(filepath.Join(config.PlaylistsPath, name), []byte(lines[0]), 0644)
		}
	}

	if len(data.History) > 0 {
		bHist, _ := json.MarshalIndent(data.History, "", "  ")
		os.WriteFile(filepath.Join("data", "deleted_history.json"), bHist, 0644)
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
