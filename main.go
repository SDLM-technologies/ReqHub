package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// --- Configuration ---
type Config struct {
	LidarrURL string `json:"lidarrUrl"`
	LidarrKey string `json:"lidarrKey"`
	MusicPath string `json:"musicPath"`
	Language  string `json:"language"`
}

var config Config
var configMutex sync.RWMutex
var pendingTracks map[int][]string
var pendingMutex sync.RWMutex

const dataDir = "data"
var configFile = filepath.Join(dataDir, "config.json")
var pendingFile = filepath.Join(dataDir, "pending_tracks.json")

// --- Initialization ---
func init() {
	os.MkdirAll(dataDir, 0755)
	pendingTracks = make(map[int][]string)
	loadConfig()
	loadPending()
}

func loadConfig() {
	configMutex.Lock()
	defer configMutex.Unlock()
	b, err := os.ReadFile(configFile)
	if err == nil {
		json.Unmarshal(b, &config)
	}
}

func saveConfig() error {
	configMutex.RLock()
	defer configMutex.RUnlock()
	b, _ := json.MarshalIndent(config, "", "  ")
	return os.WriteFile(configFile, b, 0644)
}

func loadPending() {
	pendingMutex.Lock()
	defer pendingMutex.Unlock()
	b, err := os.ReadFile(pendingFile)
	if err == nil {
		json.Unmarshal(b, &pendingTracks)
	}
}

func savePending() {
	pendingMutex.RLock()
	defer pendingMutex.RUnlock()
	b, _ := json.MarshalIndent(pendingTracks, "", "  ")
	os.WriteFile(pendingFile, b, 0644)
}

// --- HTTP Server ---
func main() {
	// Static files and web routes
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFile(w, r, "index.html")
		} else {
			http.NotFound(w, r)
		}
	})

	http.HandleFunc("/api/settings", handleSettings)
	http.HandleFunc("/api/playlists", handlePlaylists)
	http.HandleFunc("/api/search", handleSearch)
	http.HandleFunc("/api/track_status", handleTrackStatus)
	http.HandleFunc("/api/add", handleAdd)
	http.HandleFunc("/webhook", handleWebhook)
	
	http.HandleFunc("/api/playlist/create", handlePlaylistCreate)
	http.HandleFunc("/api/playlist/read", handlePlaylistRead)
	http.HandleFunc("/api/playlist/remove_track", handlePlaylistRemoveTrack)
	http.HandleFunc("/api/playlist/delete", handlePlaylistDelete)

	fmt.Println("🚀 SDLM ReqHub started on port :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// --- Handlers ---

func handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		configMutex.RLock()
		json.NewEncoder(w).Encode(config)
		configMutex.RUnlock()
		return
	}
	if r.Method == "POST" {
		var newConfig Config
		if err := json.NewDecoder(r.Body).Decode(&newConfig); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Ensure the music path ends cleanly
		newConfig.MusicPath = filepath.Clean(newConfig.MusicPath)
		configMutex.Lock()
		config = newConfig
		configMutex.Unlock()
		saveConfig()
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func handlePlaylists(w http.ResponseWriter, r *http.Request) {
	configMutex.RLock()
	musicPath := config.MusicPath
	configMutex.RUnlock()

	if musicPath == "" {
		json.NewEncoder(w).Encode([]string{})
		return
	}

	var playlists []string
	filepath.WalkDir(musicPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // Ignore access errors
		}
		ext := strings.ToLower(filepath.Ext(path))
		if !d.IsDir() && (ext == ".m3u" || ext == ".m3u8") {
			rel, _ := filepath.Rel(musicPath, path)
			playlists = append(playlists, rel)
		}
		return nil
	})

	json.NewEncoder(w).Encode(playlists)
}

// Queries the public iTunes API for quick and accurate track search
func handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		http.Error(w, "Empty search", http.StatusBadRequest)
		return
	}

	itunesURL := fmt.Sprintf("https://itunes.apple.com/search?term=%s&entity=song&limit=15", url.QueryEscape(query))
	resp, err := http.Get(itunesURL)
	if err != nil {
		http.Error(w, "iTunes API Error", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	var result struct {
		Results []map[string]interface{} `json:"results"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	// Formatting the response for the frontend
	var tracks []map[string]interface{}
	for _, item := range result.Results {
		tracks = append(tracks, map[string]interface{}{
			"title":  item["trackName"],
			"artist": item["artistName"],
			"album":  item["collectionName"],
			"cover":  item["artworkUrl100"],
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tracks)
}

func handleTrackStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req AddRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	searchTerm := fmt.Sprintf("%s %s", req.Artist, req.Album)
	lookupURL := fmt.Sprintf("/api/v1/album/lookup?term=%s", url.QueryEscape(searchTerm))
	var lookupResults []map[string]interface{}
	err := lidarrRequest("GET", lookupURL, nil, &lookupResults)
	if err != nil || len(lookupResults) == 0 {
		json.NewEncoder(w).Encode([]string{})
		return
	}

	bestAlbum := lookupResults[0]
	albumID, ok := bestAlbum["id"].(float64)
	if !ok || int(albumID) == 0 {
		json.NewEncoder(w).Encode([]string{})
		return
	}

	trackURL := fmt.Sprintf("/api/v1/track?albumId=%d", int(albumID))
	var tracks []map[string]interface{}
	err = lidarrRequest("GET", trackURL, nil, &tracks)
	if err != nil {
		json.NewEncoder(w).Encode([]string{})
		return
	}

	var targetTrack map[string]interface{}
	for _, t := range tracks {
		title := t["title"].(string)
		if strings.EqualFold(title, req.Title) || strings.Contains(strings.ToLower(title), strings.ToLower(req.Title)) {
			targetTrack = t
			break
		}
	}

	if targetTrack == nil || !targetTrack["hasFile"].(bool) {
		json.NewEncoder(w).Encode([]string{})
		return
	}

	trackFileID := int(targetTrack["trackFileId"].(float64))
	var trackFiles []map[string]interface{}
	lidarrRequest("GET", fmt.Sprintf("/api/v1/trackfile?albumId=%d", int(albumID)), nil, &trackFiles)
	
	var filePath string
	for _, tf := range trackFiles {
		if int(tf["id"].(float64)) == trackFileID {
			filePath = tf["path"].(string)
			break
		}
	}

	if filePath == "" {
		json.NewEncoder(w).Encode([]string{})
		return
	}

	configMutex.RLock()
	musicPath := config.MusicPath
	configMutex.RUnlock()

	var existingPlaylists []string
	filepath.WalkDir(musicPath, func(path string, d fs.DirEntry, err error) error {
		ext := strings.ToLower(filepath.Ext(path))
		if err != nil || d.IsDir() || (ext != ".m3u" && ext != ".m3u8") {
			return nil
		}
		
		playlistDir := filepath.Dir(path)
		relAudioPath, _ := filepath.Rel(playlistDir, filePath)
		relAudioPath = filepath.ToSlash(relAudioPath)

		content, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Split(string(content), "\n")
			for _, line := range lines {
				if strings.TrimSpace(line) == relAudioPath {
					relPlaylistPath, _ := filepath.Rel(musicPath, path)
					existingPlaylists = append(existingPlaylists, relPlaylistPath)
					break
				}
			}
		}
		return nil
	})

	json.NewEncoder(w).Encode(existingPlaylists)
}

type AddRequest struct {
	Title     string   `json:"title"`
	Artist    string   `json:"artist"`
	Album     string   `json:"album"`
	Playlists []string `json:"playlists"`
}

func handleAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req AddRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 1. Search for the album in Lidarr
	searchTerm := fmt.Sprintf("%s %s", req.Artist, req.Album)
	lookupURL := fmt.Sprintf("/api/v1/album/lookup?term=%s", url.QueryEscape(searchTerm))
	var lookupResults []map[string]interface{}
	err := lidarrRequest("GET", lookupURL, nil, &lookupResults)
	if err != nil || len(lookupResults) == 0 {
		http.Error(w, "Album not found in Lidarr/MusicBrainz", http.StatusNotFound)
		return
	}

	bestAlbum := lookupResults[0]
	albumID := int(bestAlbum["id"].(float64))

	// 2. If the album does not exist in Lidarr (ID == 0), add it
	if albumID == 0 {
		bestAlbum["addOptions"] = map[string]interface{}{
			"searchForNewAlbum": true,
		}
		var addedAlbum map[string]interface{}
		err = lidarrRequest("POST", "/api/v1/album", bestAlbum, &addedAlbum)
		if err != nil {
			http.Error(w, "Error adding album to Lidarr", http.StatusInternalServerError)
			return
		}
		if idVal, ok := addedAlbum["id"].(float64); ok {
			albumID = int(idVal)
		} else {
			http.Error(w, "Lidarr did not return a valid album ID", http.StatusInternalServerError)
			return
		}
	}

	// 3. Get album tracks
	trackURL := fmt.Sprintf("/api/v1/track?albumId=%d", albumID)
	var tracks []map[string]interface{}
	err = lidarrRequest("GET", trackURL, nil, &tracks)
	if err != nil {
		http.Error(w, "Error retrieving tracks", http.StatusInternalServerError)
		return
	}

	// Find the correct track
	var targetTrack map[string]interface{}
	for _, t := range tracks {
		title := t["title"].(string)
		if strings.EqualFold(title, req.Title) || strings.Contains(strings.ToLower(title), strings.ToLower(req.Title)) {
			targetTrack = t
			break
		}
	}

	if targetTrack == nil {
		http.Error(w, "Matching track not found in the Lidarr album", http.StatusNotFound)
		return
	}

	trackID := int(targetTrack["id"].(float64))
	hasFile := targetTrack["hasFile"].(bool)

	// 4. Scenario A (Already downloaded)
	if hasFile {
		trackFileID := int(targetTrack["trackFileId"].(float64))
		
		// Get all album files to find the correct path
		var trackFiles []map[string]interface{}
		lidarrRequest("GET", fmt.Sprintf("/api/v1/trackfile?albumId=%d", albumID), nil, &trackFiles)
		
		var filePath string
		for _, tf := range trackFiles {
			if int(tf["id"].(float64)) == trackFileID {
				filePath = tf["path"].(string)
				break
			}
		}

		if filePath != "" {
			err = syncPlaylists(filePath, req.Playlists)
			if err != nil {
				http.Error(w, "Error adding/removing playlist: "+err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "added", "message": "Playlists synchronized successfully!"})
			return
		}
	}

	// 5. Scenario B (Missing -> Pending)
	pendingMutex.Lock()
	pendingTracks[trackID] = req.Playlists
	pendingMutex.Unlock()
	savePending()

	// Trigger automatic search for the album just in case
	lidarrRequest("POST", "/api/v1/command", map[string]interface{}{
		"name": "AlbumSearch",
		"albumIds": []int{albumID},
	}, nil)

	json.NewEncoder(w).Encode(map[string]string{"status": "pending", "message": "Missing track, Lidarr download requested..."})
}

// Webhook Route
func handleWebhook(w http.ResponseWriter, r *http.Request) {
	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	eventType, _ := payload["eventType"].(string)
	if eventType == "TrackDownload" || eventType == "Download" || eventType == "Upgrade" || eventType == "TrackUpgrade" {
		// Analyze imported tracks
		tracksInterface, hasTracks := payload["tracks"].([]interface{})
		trackFile, hasFile := payload["trackFile"].(map[string]interface{})

		if hasTracks && hasFile {
			filePath := trackFile["path"].(string)
			
			pendingMutex.Lock()
			for _, tInter := range tracksInterface {
				tMap := tInter.(map[string]interface{})
				if tID, ok := tMap["id"].(float64); ok {
					trackID := int(tID)
					// If the track was pending
					if playlists, exists := pendingTracks[trackID]; exists {
						log.Printf("Pending track imported: %d -> %s\n", trackID, filePath)
						syncPlaylists(filePath, playlists)
						delete(pendingTracks, trackID)
					}
				}
			}
			pendingMutex.Unlock()
			savePending()

			// Handle Upgrades
			isUpgrade, _ := payload["isUpgrade"].(bool)
			deletedFiles, hasDeleted := payload["deletedFiles"].([]interface{})
			if isUpgrade && hasDeleted {
				for _, dInter := range deletedFiles {
					if dMap, ok := dInter.(map[string]interface{}); ok {
						if oldPath, ok := dMap["path"].(string); ok && oldPath != "" {
							log.Printf("Upgrading track path in playlists: %s -> %s\n", oldPath, filePath)
							updatePathInPlaylists(oldPath, filePath)
						}
					}
				}
			}
		}
	}

	w.WriteHeader(http.StatusOK)
}

// --- Helpers ---

// Updates the path of an audio file in all .m3u playlists (used for upgrades)
func updatePathInPlaylists(oldAudioPath, newAudioPath string) {
	configMutex.RLock()
	musicPath := config.MusicPath
	configMutex.RUnlock()

	filepath.WalkDir(musicPath, func(path string, d fs.DirEntry, err error) error {
		ext := strings.ToLower(filepath.Ext(path))
		if err != nil || d.IsDir() || (ext != ".m3u" && ext != ".m3u8") {
			return nil
		}

		playlistDir := filepath.Dir(path)
		
		relOldAudioPath, _ := filepath.Rel(playlistDir, oldAudioPath)
		relOldAudioPath = filepath.ToSlash(relOldAudioPath)
		
		relNewAudioPath, _ := filepath.Rel(playlistDir, newAudioPath)
		relNewAudioPath = filepath.ToSlash(relNewAudioPath)

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		lines := strings.Split(string(content), "\n")
		modified := false

		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == relOldAudioPath {
				lines[i] = strings.Replace(line, trimmed, relNewAudioPath, 1) // preserve leading/trailing whitespace if any
				modified = true
			}
		}

		if modified {
			out := strings.Join(lines, "\n")
			os.WriteFile(path, []byte(out), 0644)
		}

		return nil
	})
}

// Synchronizes the presence of the audio path in .m3u files
func syncPlaylists(audioPath string, checkedPlaylists []string) error {
	configMutex.RLock()
	musicPath := config.MusicPath
	configMutex.RUnlock()

	checkedMap := make(map[string]bool)
	for _, p := range checkedPlaylists {
		checkedMap[p] = true
	}

	return filepath.WalkDir(musicPath, func(path string, d fs.DirEntry, err error) error {
		ext := strings.ToLower(filepath.Ext(path))
		if err != nil || d.IsDir() || (ext != ".m3u" && ext != ".m3u8") {
			return nil
		}

		relPlaylistPath, _ := filepath.Rel(musicPath, path)
		shouldBeInPlaylist := checkedMap[relPlaylistPath]

		playlistDir := filepath.Dir(path)
		relAudioPath, _ := filepath.Rel(playlistDir, audioPath)
		relAudioPath = filepath.ToSlash(relAudioPath)

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		lines := strings.Split(string(content), "\n")
		var newLines []string
		found := false
		modified := false

		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue // Ignore existing empty lines
			}
			if trimmed == relAudioPath {
				found = true
				if shouldBeInPlaylist {
					newLines = append(newLines, line) // keep
				} else {
					modified = true // removal
				}
			} else {
				newLines = append(newLines, line) // normal line
			}
		}

		if shouldBeInPlaylist && !found {
			newLines = append(newLines, relAudioPath) // addition
			modified = true
		}

		if modified {
			// Add an empty line at the end to keep it clean
			out := strings.Join(newLines, "\n")
			if !strings.HasSuffix(out, "\n") {
				out += "\n"
			}
			os.WriteFile(path, []byte(out), 0644)
		}

		return nil
	})
}

// Performs an HTTP request to the Lidarr API
func lidarrRequest(method, endpoint string, body interface{}, out interface{}) error {
	configMutex.RLock()
	lURL := strings.TrimRight(config.LidarrURL, "/")
	lKey := config.LidarrKey
	configMutex.RUnlock()

	var reqBody io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reqBody = bytes.NewBuffer(b)
	}

	req, err := http.NewRequest(method, lURL+endpoint, reqBody)
	if err != nil {
		return err
	}
	
	req.Header.Set("X-Api-Key", lKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("lidarr error: HTTP %d", resp.StatusCode)
	}

	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// --- Playlist CRUD ---

type CreatePlaylistReq struct {
	Name string `json:"name"`
}

func handlePlaylistCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req CreatePlaylistReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || strings.Contains(name, "..") || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		http.Error(w, "Invalid name", http.StatusBadRequest)
		return
	}
	lowName := strings.ToLower(name)
	if !strings.HasSuffix(lowName, ".m3u") && !strings.HasSuffix(lowName, ".m3u8") {
		name += ".m3u8"
	}
	
	configMutex.RLock()
	musicPath := config.MusicPath
	configMutex.RUnlock()

	fullPath := filepath.Join(musicPath, name)
	
	if _, err := os.Stat(fullPath); err == nil {
		http.Error(w, "Playlist already exists", http.StatusConflict)
		return
	}

	if err := os.WriteFile(fullPath, []byte(""), 0644); err != nil {
		http.Error(w, "Error creating file", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

type PlaylistItem struct {
	Line    string `json:"line"`
	Display string `json:"display"`
}

func handlePlaylistRead(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" || strings.Contains(name, "..") {
		http.Error(w, "Invalid name", http.StatusBadRequest)
		return
	}

	configMutex.RLock()
	musicPath := config.MusicPath
	configMutex.RUnlock()

	fullPath := filepath.Join(musicPath, name)

	content, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "Playlist not found", http.StatusNotFound)
		} else {
			http.Error(w, "Read error", http.StatusInternalServerError)
		}
		return
	}

	lines := strings.Split(string(content), "\n")
	var items []PlaylistItem
	var lastExtinf string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#EXTINF:") {
			parts := strings.SplitN(trimmed, ",", 2)
			if len(parts) == 2 {
				lastExtinf = strings.TrimSpace(parts[1])
			}
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}

		display := lastExtinf
		if display == "" {
			base := filepath.Base(trimmed)
			display = strings.TrimSuffix(base, filepath.Ext(base))
		}
		
		items = append(items, PlaylistItem{
			Line:    trimmed,
			Display: display,
		})
		lastExtinf = ""
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}

type RemoveTrackReq struct {
	Playlist string `json:"playlist"`
	Line     string `json:"line"`
}

func handlePlaylistRemoveTrack(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req RemoveTrackReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Playlist, "..") {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	configMutex.RLock()
	musicPath := config.MusicPath
	configMutex.RUnlock()

	fullPath := filepath.Join(musicPath, req.Playlist)

	content, err := os.ReadFile(fullPath)
	if err != nil {
		http.Error(w, "Read error", http.StatusInternalServerError)
		return
	}

	lines := strings.Split(string(content), "\n")
	var newLines []string
	
	skipNext := false
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if skipNext && strings.HasPrefix(trimmed, "#EXTINF:") {
			skipNext = false
			continue
		}
		skipNext = false

		if trimmed == req.Line {
			skipNext = true
			continue
		}
		newLines = append([]string{lines[i]}, newLines...)
	}

	out := strings.Join(newLines, "\n")
	if len(out) > 0 && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}

	if err := os.WriteFile(fullPath, []byte(out), 0644); err != nil {
		http.Error(w, "Write error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

type DeletePlaylistReq struct {
	Playlist string `json:"playlist"`
}

func handlePlaylistDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req DeletePlaylistReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Playlist, "..") {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	configMutex.RLock()
	musicPath := config.MusicPath
	configMutex.RUnlock()

	fullPath := filepath.Join(musicPath, req.Playlist)

	if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
		http.Error(w, "Delete error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
