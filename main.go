package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"github.com/dhowden/tag"
)

// --- Configuration Structure ---
// Config holds the application settings.
// This struct maps directly to the JSON configuration file used by the application.
type Config struct {
	LidarrURL     string `json:"lidarrUrl"`     // Base URL of the Lidarr instance (e.g., http://192.168.1.10:8686)
	LidarrKey     string `json:"lidarrKey"`     // API Key for authenticating with Lidarr
	MusicPath     string `json:"musicPath"`     // Absolute path on the host to the music library directory
	PlaylistsPath string `json:"playlistsPath"` // Absolute path on the host to the directory where .m3u8 playlists are stored
	NaviUrl       string `json:"naviUrl"`       // Base URL of the Navidrome instance (optional)
	NaviUser      string `json:"naviUser"`      // Navidrome username (optional)
	NaviPass      string `json:"naviPass"`      // Navidrome password (optional)
	NaviKey       string `json:"naviKey"`       // Navidrome generated token/salt for API auth (optional)
	Language      string `json:"language"`      // Application language preference (e.g., 'en', 'fr')
}

// Global state variables
var config Config
var configMutex sync.RWMutex // Protects concurrent access to the config map

// pendingTracks stores tracks that are requested but not yet downloaded by Lidarr.
// Key: Lidarr Track ID, Value: List of playlist names where the track should be added once downloaded.
var pendingTracks map[int][]string
var pendingMutex sync.RWMutex // Protects concurrent access to pendingTracks

const dataDir = "data"
var configFile = filepath.Join(dataDir, "config.json")
var pendingFile = filepath.Join(dataDir, "pending_tracks.json")

// --- Initialization ---
// init is called automatically upon application startup.
// It ensures the data directory exists and loads the configuration and pending tracks from disk.
func init() {
	os.MkdirAll(dataDir, 0755)
	pendingTracks = make(map[int][]string)
	loadConfig()
	loadPending()
}

// loadConfig reads the configuration file from disk and populates the global config struct.
func loadConfig() {
	configMutex.Lock()
	defer configMutex.Unlock()
	b, err := os.ReadFile(configFile)
	if err == nil {
		json.Unmarshal(b, &config)
	}
}

// saveConfig serializes the global config struct to JSON and writes it to disk.
func saveConfig() error {
	configMutex.RLock()
	defer configMutex.RUnlock()
	b, _ := json.MarshalIndent(config, "", "  ")
	return os.WriteFile(configFile, b, 0644)
}

// loadPending reads the pending tracks from disk, restoring the application's state after a restart.
func loadPending() {
	pendingMutex.Lock()
	defer pendingMutex.Unlock()
	b, err := os.ReadFile(pendingFile)
	if err == nil {
		json.Unmarshal(b, &pendingTracks)
	}
}

// savePending serializes the pending tracks state to JSON and writes it to disk.
func savePending() {
	pendingMutex.RLock()
	defer pendingMutex.RUnlock()
	b, _ := json.MarshalIndent(pendingTracks, "", "  ")
	os.WriteFile(pendingFile, b, 0644)
}

// --- HTTP Server ---
func main() {
	// Serve static files and handle root web route
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFile(w, r, "index.html")
		} else {
			http.NotFound(w, r)
		}
	})

	// Register API endpoints
	http.HandleFunc("/api/settings", handleSettings)
	http.HandleFunc("/api/playlists", handlePlaylists)
	http.HandleFunc("/api/search", handleSearch)
	http.HandleFunc("/api/track_status", handleTrackStatus)
	http.HandleFunc("/api/add", handleAdd)
	
	// Webhook endpoint to receive events from Lidarr (e.g., track imported)
	http.HandleFunc("/webhook", handleWebhook)
	
	// Playlist management endpoints
	http.HandleFunc("/api/playlist/create", handlePlaylistCreate)
	http.HandleFunc("/api/playlist/read", handlePlaylistRead)
	http.HandleFunc("/api/playlist/remove_track", handlePlaylistRemoveTrack)
	http.HandleFunc("/api/playlist/delete", handlePlaylistDelete)
	
	// Cover art endpoints
	http.HandleFunc("/api/cover", handleCover)
	http.HandleFunc("/api/playlist/cover", handlePlaylistCover)

	fmt.Println("🚀 SDLM ReqHub started on port :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// --- Route Handlers ---

// handleSettings manages reading (GET) and updating (POST) the application configuration.
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
		// Ensure the paths are clean and do not contain trailing slashes or relative segments
		newConfig.MusicPath = filepath.Clean(newConfig.MusicPath)
		newConfig.PlaylistsPath = filepath.Clean(newConfig.PlaylistsPath)
		// Fallback: If playlists path is empty, default it to the music path
		if newConfig.PlaylistsPath == "" {
			newConfig.PlaylistsPath = newConfig.MusicPath
		}
		
		configMutex.Lock()
		config = newConfig
		configMutex.Unlock()
		
		saveConfig()
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// handleCover serves album cover images for individual tracks.
// It securely resolves the path for the requested image either relative to the playlist file,
// or directly from the music directory if the path is absolute or not tied to a playlist.
func handleCover(w http.ResponseWriter, r *http.Request) {
	audioPath := r.URL.Query().Get("path")
	playlistName := r.URL.Query().Get("playlist")
	if audioPath == "" {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	configMutex.RLock()
	musicPath := config.MusicPath
	playlistsPath := config.PlaylistsPath
	configMutex.RUnlock()

	var fullAudioPath string
	
	artist := r.URL.Query().Get("artist")
	album := r.URL.Query().Get("album")

	// Determine the absolute path of the audio file to locate its directory
	if filepath.IsAbs(audioPath) {
		// Use absolute path directly if provided in the m3u8 file
		fullAudioPath = audioPath
	} else if playlistName != "" && !strings.Contains(playlistName, "..") {
		// Resolve relative path based on the playlist file's directory
		playlistDir := filepath.Dir(filepath.Join(playlistsPath, playlistName))
		fullAudioPath = filepath.Join(playlistDir, audioPath)
	} else {
		// Fallback: Resolve against the global music path root
		if strings.Contains(audioPath, "..") {
			http.Error(w, "Invalid path: cannot use relative navigation outside a playlist context", http.StatusBadRequest)
			return
		}
		fullAudioPath = filepath.Join(musicPath, audioPath)
	}
	
	// Step 1: Try to read embedded cover art directly from the audio file tags (ID3/FLAC)
	if f, err := os.Open(fullAudioPath); err == nil {
		if m, err := tag.ReadFrom(f); err == nil {
			if pic := m.Picture(); pic != nil && len(pic.Data) > 0 {
				w.Header().Set("Content-Type", pic.MIMEType)
				w.Write(pic.Data)
				f.Close()
				return
			}
		}
		f.Close()
	}

	dir := filepath.Dir(fullAudioPath)

	// Step 2: List of common cover art filenames used by Lidarr and other media managers
	// Both lowercase and Capitalized versions are included for Linux filesystem compatibility
	covers := []string{
		"cover.jpg", "cover.png", "folder.jpg", "folder.png", 
		"Cover.jpg", "Cover.png", "Folder.jpg", "Folder.png", 
		"cover.JPG", "folder.JPG",
	}
	
	for _, c := range covers {
		coverPath := filepath.Join(dir, c)
		if _, err := os.Stat(coverPath); err == nil {
			http.ServeFile(w, r, coverPath)
			return
		}
	}

	// Step 2: Fallback to Lidarr API if artist and album are provided and no local file exists
	if artist != "" && album != "" {
		searchTerm := fmt.Sprintf("%s %s", artist, album)
		lookupURL := fmt.Sprintf("/api/v1/album/lookup?term=%s", url.QueryEscape(searchTerm))
		var lookupResults []map[string]interface{}
		
		err := lidarrRequest("GET", lookupURL, nil, &lookupResults)
		if err == nil && len(lookupResults) > 0 {
			bestAlbum := lookupResults[0]
			if images, ok := bestAlbum["images"].([]interface{}); ok {
				for _, imgInterface := range images {
					if img, ok := imgInterface.(map[string]interface{}); ok {
						if coverType, _ := img["coverType"].(string); coverType == "cover" {
							if imgURL, _ := img["url"].(string); imgURL != "" {
								// Fetch image from Lidarr
								configMutex.RLock()
								lURL := strings.TrimRight(config.LidarrURL, "/")
								lKey := config.LidarrKey
								configMutex.RUnlock()

								req, _ := http.NewRequest("GET", lURL+imgURL, nil)
								req.Header.Set("X-Api-Key", lKey)
								
								resp, err := http.DefaultClient.Do(req)
								if err == nil && resp.StatusCode == 200 {
									defer resp.Body.Close()
									w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
									io.Copy(w, resp.Body)
									return
								}
							}
						}
					}
				}
			}
		}
	}

	http.Error(w, "Cover not found", http.StatusNotFound)
}

// handlePlaylistCover manages reading (GET) and updating (POST) custom cover images for entire playlists.
func handlePlaylistCover(w http.ResponseWriter, r *http.Request) {
	playlistName := r.URL.Query().Get("name")
	if playlistName == "" || strings.Contains(playlistName, "..") {
		http.Error(w, "Invalid playlist name", http.StatusBadRequest)
		return
	}

	configMutex.RLock()
	playlistsPath := config.PlaylistsPath
	configMutex.RUnlock()

	playlistDir := filepath.Dir(filepath.Join(playlistsPath, playlistName))

	if r.Method == "GET" {
		// Attempt to locate a custom cover image in the playlist's directory
		covers := []string{"cover.jpg", "cover.png", playlistName + "_cover.jpg", playlistName + "_cover.png"}
		
		// If the playlist is in the root directory, check for a uniquely named cover file
		// to avoid conflicts with other root playlists (e.g. "myplaylist_cover.jpg")
		baseName := filepath.Base(playlistName)
		if ext := filepath.Ext(baseName); ext != "" {
			baseName = strings.TrimSuffix(baseName, ext)
		}
		covers = append(covers, baseName+"_cover.jpg", baseName+"_cover.png")
		
		for _, c := range covers {
			coverPath := filepath.Join(playlistDir, c)
			if _, err := os.Stat(coverPath); err == nil && !strings.Contains(coverPath, "..") {
				http.ServeFile(w, r, coverPath)
				return
			}
		}
		http.Error(w, "Custom playlist cover not found", http.StatusNotFound)
		return
	}

	if r.Method == "POST" {
		defer r.Body.Close()
		content, err := io.ReadAll(r.Body)
		if err != nil || len(content) == 0 {
			http.Error(w, "Invalid body content", http.StatusBadRequest)
			return
		}
		
		baseName := filepath.Base(playlistName)
		if ext := filepath.Ext(baseName); ext != "" {
			baseName = strings.TrimSuffix(baseName, ext)
		}
		
		coverFileName := "cover.jpg"
		// If the playlist is in the root directory (PlaylistsPath), save it as "basename_cover.jpg"
		// to prevent overwriting covers of other root playlists.
		if playlistDir == playlistsPath {
			coverFileName = baseName + "_cover.jpg"
		}
		
		coverPath := filepath.Join(playlistDir, coverFileName)
		err = os.WriteFile(coverPath, content, 0644)
		if err != nil {
			http.Error(w, "Failed to write cover image to disk", http.StatusInternalServerError)
			return
		}
		
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		return
	}
	
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// PlaylistInfo represents metadata about a playlist returned to the frontend frontend.
type PlaylistInfo struct {
	Name        string   `json:"name"`        // Relative path/name of the playlist file
	CustomCover string   `json:"customCover"` // URL to the custom cover image (if one exists)
	TrackCovers []string `json:"trackCovers"` // URLs to the first 4 track covers (used to generate a 2x2 grid preview if no custom cover exists)
}

// handlePlaylists scans the configured playlists directory and returns a list of all available playlists,
// including preview images (either a custom cover or a grid of the first 4 tracks).
func handlePlaylists(w http.ResponseWriter, r *http.Request) {
	configMutex.RLock()
	playlistsPath := config.PlaylistsPath
	configMutex.RUnlock()

	// If no playlist path is configured, return an empty array
	if playlistsPath == "" {
		json.NewEncoder(w).Encode([]PlaylistInfo{})
		return
	}

	var playlists []PlaylistInfo
	// Recursively walk the playlist directory to find all .m3u and .m3u8 files
	filepath.WalkDir(playlistsPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // Safely ignore access/permission errors
		}
		
		ext := strings.ToLower(filepath.Ext(path))
		if !d.IsDir() && (ext == ".m3u" || ext == ".m3u8") {
			// Determine the playlist name relative to the base playlist path
			rel, _ := filepath.Rel(playlistsPath, path)
			info := PlaylistInfo{Name: rel}
			
			// Extract the directory and base filename (without extension) for this specific playlist
			playlistDir := filepath.Dir(path)
			baseName := filepath.Base(rel)
			if ext := filepath.Ext(baseName); ext != "" {
				baseName = strings.TrimSuffix(baseName, ext)
			}
			
			customCoverFound := false
			// Check if a custom cover image exists for this playlist
			covers := []string{"cover.jpg", "cover.png"}
			// To avoid conflicts in the root directory, root playlists look for "playlistname_cover.jpg"
			if playlistDir == playlistsPath {
				covers = []string{baseName + "_cover.jpg", baseName + "_cover.png"}
			}
			
			for _, c := range covers {
				if _, err := os.Stat(filepath.Join(playlistDir, c)); err == nil {
					info.CustomCover = fmt.Sprintf("/api/playlist/cover?name=%s", url.QueryEscape(rel))
					customCoverFound = true
					break
				}
			}
			
			// If no custom cover is found, read the first 4 audio tracks from the playlist file
			// to generate a 2x2 grid image preview in the UI.
			if !customCoverFound {
				if content, err := os.ReadFile(path); err == nil {
					lines := strings.Split(string(content), "\n")
					var trackCovers []string
					for _, line := range lines {
						trimmed := strings.TrimSpace(line)
						// Ignore empty lines and # comments/metadata
						if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
							trackCovers = append(trackCovers, fmt.Sprintf("/api/cover?path=%s&playlist=%s", url.QueryEscape(trimmed), url.QueryEscape(rel)))
							if len(trackCovers) == 4 {
								break
							}
						}
					}
					info.TrackCovers = trackCovers
				}
			}
			playlists = append(playlists, info)
		}
		return nil
	})

	json.NewEncoder(w).Encode(playlists)
}

// handleSearch queries the public iTunes Search API to provide fast, accurate autocomplete
// results when the user is searching for a song to request.
func handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		http.Error(w, "Empty search query", http.StatusBadRequest)
		return
	}

	// Request up to 15 matching songs from iTunes API
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

	// Format the raw iTunes API response into a simplified structure for the frontend UI
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

// handleTrackStatus checks the current status of a specific track in Lidarr.
// It verifies if the track is already available in the library and returns a list
// of playlists that currently include this track.
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

	// First, lookup the album in Lidarr to get its internal ID
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

	// Fetch all tracks for the found album to find the specific requested track
	trackURL := fmt.Sprintf("/api/v1/track?albumId=%d", int(albumID))
	var tracks []map[string]interface{}
	err = lidarrRequest("GET", trackURL, nil, &tracks)
	if err != nil {
		json.NewEncoder(w).Encode([]string{})
		return
	}

	// Find the correct track by fuzzy matching the title
	var targetTrack map[string]interface{}
	for _, t := range tracks {
		title := t["title"].(string)
		if strings.EqualFold(title, req.Title) || strings.Contains(strings.ToLower(title), strings.ToLower(req.Title)) {
			targetTrack = t
			break
		}
	}

	// If the track isn't found or hasn't been downloaded yet, return an empty array
	if targetTrack == nil || !targetTrack["hasFile"].(bool) {
		json.NewEncoder(w).Encode([]string{})
		return
	}

	// If the file exists, locate its exact filepath by resolving the trackFileId
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

	// Finally, scan the local playlists directory to see which playlists currently include this file
	configMutex.RLock()
	playlistsPath := config.PlaylistsPath
	configMutex.RUnlock()

	var existingPlaylists []string
	filepath.WalkDir(playlistsPath, func(path string, d fs.DirEntry, err error) error {
		ext := strings.ToLower(filepath.Ext(path))
		if err != nil || d.IsDir() || (ext != ".m3u" && ext != ".m3u8") {
			return nil
		}
		
		// Calculate the expected relative path as it would appear in the .m3u8 file
		playlistDir := filepath.Dir(path)
		relAudioPath, _ := filepath.Rel(playlistDir, filePath)
		relAudioPath = filepath.ToSlash(relAudioPath)

		content, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Split(string(content), "\n")
			for _, line := range lines {
				if strings.TrimSpace(line) == relAudioPath {
					relPlaylistPath, _ := filepath.Rel(playlistsPath, path)
					existingPlaylists = append(existingPlaylists, relPlaylistPath)
					break
				}
			}
		}
		return nil
	})

	json.NewEncoder(w).Encode(existingPlaylists)
}

// AddRequest represents the JSON payload when a user requests a song to be downloaded/added.
type AddRequest struct {
	Title     string   `json:"title"`
	Artist    string   `json:"artist"`
	Album     string   `json:"album"`
	Playlists []string `json:"playlists"`
}

// handleAdd processes user requests to add a track to one or more playlists.
// If the track is missing in Lidarr, it requests a download and adds it to the 'pending' list.
// If the track is already available, it immediately adds the track to the requested playlists.
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

	// Step 1: Search for the album in Lidarr using the artist and album name
	searchTerm := fmt.Sprintf("%s %s", req.Artist, req.Album)
	lookupURL := fmt.Sprintf("/api/v1/album/lookup?term=%s", url.QueryEscape(searchTerm))
	var lookupResults []map[string]interface{}
	err := lidarrRequest("GET", lookupURL, nil, &lookupResults)
	if err != nil || len(lookupResults) == 0 {
		http.Error(w, "Album not found in Lidarr/MusicBrainz", http.StatusNotFound)
		return
	}

	bestAlbum := lookupResults[0]
	
	// Safely extract the album ID. If it's missing (unmonitored album), it will be 0.
	albumID := 0
	if idVal, ok := bestAlbum["id"].(float64); ok {
		albumID = int(idVal)
	}

	// Step 2: If the album does not exist in Lidarr's monitored library (ID == 0), add it
	if albumID == 0 {
		bestAlbum["addOptions"] = map[string]interface{}{
			// Setting to false prevents 500 Server Error if user has no download client connected in Lidarr.
			// The asynchronous search command triggered at the end of this function handles the search anyway!
			"searchForNewAlbum": false,
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

	// Step 3: Fetch the tracks for the monitored album to identify the requested track ID
	trackURL := fmt.Sprintf("/api/v1/track?albumId=%d", albumID)
	var tracks []map[string]interface{}
	err = lidarrRequest("GET", trackURL, nil, &tracks)
	if err != nil {
		http.Error(w, "Error retrieving tracks from Lidarr", http.StatusInternalServerError)
		return
	}

	// Fuzzy match the requested title against Lidarr's track metadata
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

	// Step 4: Scenario A - The track is already downloaded. Add it to playlists directly.
	if hasFile {
		trackFileID := int(targetTrack["trackFileId"].(float64))
		
		// Fetch all file paths for the album to locate the exact path of the track
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

// handleWebhook receives real-time events triggered by Lidarr (e.g., when a track is downloaded or upgraded).
func handleWebhook(w http.ResponseWriter, r *http.Request) {
	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	eventType, _ := payload["eventType"].(string)
	// We are interested in events that indicate a new or updated audio file
	if eventType == "TrackDownload" || eventType == "Download" || eventType == "Upgrade" || eventType == "TrackUpgrade" {
		
		// Parse the tracks and track file details from the webhook payload
		tracksInterface, hasTracks := payload["tracks"].([]interface{})
		trackFile, hasFile := payload["trackFile"].(map[string]interface{})

		if hasTracks && hasFile {
			filePath := trackFile["path"].(string)
			
			pendingMutex.Lock()
			for _, tInter := range tracksInterface {
				tMap := tInter.(map[string]interface{})
				if tID, ok := tMap["id"].(float64); ok {
					trackID := int(tID)
					// Check if this newly downloaded track was previously requested and queued in pendingTracks
					if playlists, exists := pendingTracks[trackID]; exists {
						log.Printf("Pending track imported: %d -> %s\n", trackID, filePath)
						// Add the newly downloaded track to the requested playlists
						syncPlaylists(filePath, playlists)
						// Remove it from the pending list
						delete(pendingTracks, trackID)
					}
				}
			}
			pendingMutex.Unlock()
			savePending()

			// Handle track upgrades (e.g., a higher quality version replaced an older version)
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

			// Automatically trigger a Navidrome scan so the new files appear immediately in the UI
			go startNavidromeScan()
		}
	}

	// Always return 200 OK so Lidarr knows the webhook was received
	w.WriteHeader(http.StatusOK)
}

// --- Helpers ---

// updatePathInPlaylists iterates through all .m3u/.m3u8 files and replaces instances of an old audio path
// with a new audio path. This is crucial for handling file upgrades (e.g., MP3 -> FLAC) triggered by Lidarr.
func updatePathInPlaylists(oldAudioPath, newAudioPath string) {
	configMutex.RLock()
	playlistsPath := config.PlaylistsPath
	configMutex.RUnlock()

	filepath.WalkDir(playlistsPath, func(path string, d fs.DirEntry, err error) error {
		ext := strings.ToLower(filepath.Ext(path))
		if err != nil || d.IsDir() || (ext != ".m3u" && ext != ".m3u8") {
			return nil
		}

		playlistDir := filepath.Dir(path)
		
		// Convert absolute paths to relative paths as they appear in the playlist files
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

		// Search and replace the specific old path with the new path
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

// syncPlaylists ensures a specific audio file is present ONLY in the playlists specified in `checkedPlaylists`.
// If the track is not present in a requested playlist, it is appended.
// If the track is present in an unchecked playlist, it is removed.
func syncPlaylists(audioPath string, checkedPlaylists []string) error {
	configMutex.RLock()
	playlistsPath := config.PlaylistsPath
	configMutex.RUnlock()

	// Convert the requested playlist slice into a map for fast lookup O(1)
	checkedMap := make(map[string]bool)
	for _, p := range checkedPlaylists {
		checkedMap[p] = true
	}

	// Recursively walk through all playlist files
	return filepath.WalkDir(playlistsPath, func(path string, d fs.DirEntry, err error) error {
		ext := strings.ToLower(filepath.Ext(path))
		if err != nil || d.IsDir() || (ext != ".m3u" && ext != ".m3u8") {
			return nil
		}

		relPlaylistPath, _ := filepath.Rel(playlistsPath, path)
		shouldBeInPlaylist := checkedMap[relPlaylistPath]

		playlistDir := filepath.Dir(path)
		// Calculate the relative path of the audio file from this specific playlist's directory
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
		hasExtM3u := false

		// Parse the existing playlist file line by line
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue // Ignore existing empty lines to clean up the file
			}
			if strings.HasPrefix(trimmed, "#EXTM3U") {
				hasExtM3u = true
			}
			
			// Check if the current line points to the audio track we are synchronizing
			if trimmed == relAudioPath {
				found = true
				if shouldBeInPlaylist {
					newLines = append(newLines, line) // Keep it in the playlist
				} else {
					modified = true // Remove it from the playlist
				}
			} else {
				newLines = append(newLines, line) // Keep all other tracks and comments
			}
		}

		// If the track is supposed to be in the playlist but wasn't found, add it
		if shouldBeInPlaylist && !found {
			newLines = append(newLines, relAudioPath) // Append the track path
			modified = true
		}

		// Ensure the playlist has the required M3U header
		if !hasExtM3u {
			newLines = append([]string{"#EXTM3U"}, newLines...)
			modified = true
		}

		// If we made changes (added, removed, or formatted), save the updated playlist
		if modified {
			// Add an empty line at the end to keep the file clean and POSIX compliant
			out := strings.Join(newLines, "\n")
			if !strings.HasSuffix(out, "\n") {
				out += "\n"
			}
			os.WriteFile(path, []byte(out), 0644)
		}

		return nil
	})
}

// lidarrRequest is a generic helper function to perform API calls to Lidarr.
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

// --- Playlist CRUD Operations ---

// CreatePlaylistReq represents the incoming JSON request to create a new playlist.
type CreatePlaylistReq struct {
	Name string `json:"name"`
}

// handlePlaylistCreate creates a new playlist directory and an empty .m3u8 file.
// It ensures that playlists are organized into folders to allow custom cover images.
func handlePlaylistCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req CreatePlaylistReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}
	
	name := strings.TrimSpace(req.Name)
	// Basic security check to prevent directory traversal or malformed paths
	if name == "" || strings.Contains(name, "..") || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		http.Error(w, "Invalid playlist name provided", http.StatusBadRequest)
		return
	}
	
	// Determine the base name (folder name) and the file name
	baseName := name
	lowName := strings.ToLower(name)
	if strings.HasSuffix(lowName, ".m3u") || strings.HasSuffix(lowName, ".m3u8") {
		baseName = name[:strings.LastIndex(name, ".")]
	} else {
		name += ".m3u8"
	}
	
	configMutex.RLock()
	playlistsPath := config.PlaylistsPath
	configMutex.RUnlock()

	dirPath := filepath.Join(playlistsPath, baseName)
	fullPath := filepath.Join(dirPath, name)
	
	// Create the dedicated folder for the playlist
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		http.Error(w, "Failed to create playlist directory", http.StatusInternalServerError)
		return
	}

	// Prevent overwriting existing playlists
	if _, err := os.Stat(fullPath); err == nil {
		http.Error(w, "Playlist file already exists", http.StatusConflict)
		return
	}

	// Initialize the file with the required M3U header
	if err := os.WriteFile(fullPath, []byte("#EXTM3U\n"), 0644); err != nil {
		http.Error(w, "Failed to write playlist file", http.StatusInternalServerError)
		return
	}
	
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// PlaylistItem represents a single audio track inside a playlist, returned to the frontend.
type PlaylistItem struct {
	Line    string `json:"line"`    // The raw path as written in the .m3u8 file
	Display string `json:"display"` // Legacy property for backwards compatibility
	Cover   string `json:"cover"`   // URL to fetch the album cover for this specific track
	Title   string `json:"title"`   // Track title
	Artist  string `json:"artist"`  // Artist name
	Album   string `json:"album"`   // Album name
}

// handlePlaylistRead reads the contents of a specific playlist and extracts metadata
// for each track to display in the UI.
func handlePlaylistRead(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" || strings.Contains(name, "..") {
		http.Error(w, "Invalid playlist name provided", http.StatusBadRequest)
		return
	}

	configMutex.RLock()
	playlistsPath := config.PlaylistsPath
	configMutex.RUnlock()

	fullPath := filepath.Join(playlistsPath, name)

	content, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "Playlist file not found", http.StatusNotFound)
		} else {
			http.Error(w, "Failed to read playlist file", http.StatusInternalServerError)
		}
		return
	}

	lines := strings.Split(string(content), "\n")
	var items []PlaylistItem
	var lastExtinf string

	// Iterate through the playlist line by line to extract track paths and metadata
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		
		// Skip empty lines
		if trimmed == "" {
			continue
		}
		
		// If the line is a metadata tag (#EXTINF), extract the track information.
		// Standard format: #EXTINF:123,Artist - Album - Title
		if strings.HasPrefix(trimmed, "#EXTINF:") {
			parts := strings.SplitN(trimmed, ",", 2)
			if len(parts) == 2 {
				lastExtinf = strings.TrimSpace(parts[1])
			}
			continue
		}
		
		// Skip any other comments or unsupported tags
		if strings.HasPrefix(trimmed, "#") {
			continue
		}

		// At this point, the line is a file path to an audio track
		// Determine absolute path of the audio file
		var fullAudioPath string
		if filepath.IsAbs(trimmed) {
			fullAudioPath = trimmed
		} else {
			playlistDir := filepath.Dir(fullPath)
			fullAudioPath = filepath.Join(playlistDir, trimmed)
		}

		title, artist, album := "", "", ""
		
		// Attempt to read metadata tags (ID3/FLAC) directly from the local file
		if f, err := os.Open(fullAudioPath); err == nil {
			if m, err := tag.ReadFrom(f); err == nil {
				title = m.Title()
				artist = m.Artist()
				if artist == "" {
					artist = m.AlbumArtist()
				}
				album = m.Album()
			}
			f.Close()
		}

		// If the file doesn't exist or tags are incomplete, fallback to extracting from EXTINF / filename
		if title == "" || artist == "" || album == "" {
			// 1. Try to extract metadata from the preceding #EXTINF tag if one existed
			if lastExtinf != "" {
				parts := strings.Split(lastExtinf, " - ")
				if len(parts) >= 3 {
					if artist == "" { artist = strings.TrimSpace(parts[0]) }
					if album == "" { album = strings.TrimSpace(parts[1]) }
					if title == "" { title = strings.TrimSpace(parts[2]) }
				} else if len(parts) == 2 {
					if artist == "" { artist = strings.TrimSpace(parts[0]) }
					if title == "" { title = strings.TrimSpace(parts[1]) }
				} else {
					if title == "" { title = lastExtinf }
				}
			}

		// 2. If no title was found in the metadata, use the file name
		if title == "" {
			base := filepath.Base(trimmed)
			title = strings.TrimSuffix(base, filepath.Ext(base))
		}
		
		// 3. Fallback: If artist or album are missing, try to guess them from the directory structure.
		// (e.g., .../Artist/Album/Track.mp3)
		if artist == "" || album == "" {
			parts := strings.Split(filepath.ToSlash(trimmed), "/")
			if len(parts) >= 3 {
				if artist == "" { artist = parts[len(parts)-3] }
				if album == "" { album = parts[len(parts)-2] }
			}
		}
		}

		// Clean up the extracted metadata to ensure clean UI and accurate Lidarr searches
		cleanRegex := regexp.MustCompile(`(?i)\s*\[.*?\]|\s*\(.*?\)`)
		trackNumRegex := regexp.MustCompile(`^\d+\s*-\s*|^\d+\s+`)
		
		title = trackNumRegex.ReplaceAllString(title, "")
		title = cleanRegex.ReplaceAllString(title, "")
		
		// Frequently Lidarr leaves the Artist and Album in the file name even after splitting:
		if artist != "" {
			title = strings.TrimPrefix(title, artist+" - ")
		}
		if album != "" {
			title = strings.TrimPrefix(title, album+" - ")
		}
		
		title = strings.TrimSpace(title)

		artist = cleanRegex.ReplaceAllString(artist, "")
		artist = strings.TrimSpace(artist)

		album = cleanRegex.ReplaceAllString(album, "")
		album = strings.TrimSpace(album)

		// Create a user-friendly display string
		display := title
		if artist != "" {
			display = artist + " - " + title
		}

		items = append(items, PlaylistItem{
			Line:    trimmed,
			Display: display, // Kept for backwards compatibility
			Title:   title,
			Artist:  artist,
			Album:   album,
			Cover:   fmt.Sprintf("/api/cover?path=%s&playlist=%s&artist=%s&album=%s", url.QueryEscape(trimmed), url.QueryEscape(name), url.QueryEscape(artist), url.QueryEscape(album)),
		})
		
		// Reset the metadata for the next track
		lastExtinf = ""
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}

// RemoveTrackReq represents the JSON payload to remove a specific track from a playlist.
type RemoveTrackReq struct {
	Playlist string `json:"playlist"` // The name/path of the playlist
	Line     string `json:"line"`     // The exact path string of the track to remove as it appears in the .m3u8 file
}

// handlePlaylistRemoveTrack handles requests to remove a single track from a playlist file.
func handlePlaylistRemoveTrack(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req RemoveTrackReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Playlist, "..") {
		http.Error(w, "Invalid playlist path", http.StatusBadRequest)
		return
	}

	configMutex.RLock()
	playlistsPath := config.PlaylistsPath
	configMutex.RUnlock()

	fullPath := filepath.Join(playlistsPath, req.Playlist)

	content, err := os.ReadFile(fullPath)
	if err != nil {
		http.Error(w, "Failed to read playlist file", http.StatusInternalServerError)
		return
	}

	lines := strings.Split(string(content), "\n")
	var newLines []string
	
	// Iterate backwards to easily remove the #EXTINF tag immediately preceding the targeted track
	skipNext := false
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		
		// If we skipped the track in the previous iteration, this line should be its #EXTINF metadata.
		// Skip it as well to keep the playlist clean.
		if skipNext && strings.HasPrefix(trimmed, "#EXTINF:") {
			skipNext = false
			continue
		}
		skipNext = false

		// Identify the track to remove
		if trimmed == req.Line {
			skipNext = true // Signal to skip the #EXTINF metadata on the next loop iteration (which is the preceding line)
			continue
		}
		
		// Prepend to maintain original order since we are iterating backwards
		newLines = append([]string{lines[i]}, newLines...)
	}

	out := strings.Join(newLines, "\n")
	// Ensure the file ends with a clean newline character
	if len(out) > 0 && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}

	if err := os.WriteFile(fullPath, []byte(out), 0644); err != nil {
		http.Error(w, "Failed to update playlist file", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// DeletePlaylistReq represents the JSON payload to delete an entire playlist.
type DeletePlaylistReq struct {
	Playlist string `json:"playlist"` // The name/path of the playlist to delete
}

// handlePlaylistDelete completely removes a playlist and its containing directory (if empty).
// It also attempts to delete the playlist from the integrated Navidrome instance.
func handlePlaylistDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req DeletePlaylistReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Playlist, "..") {
		http.Error(w, "Invalid playlist path", http.StatusBadRequest)
		return
	}

	configMutex.RLock()
	playlistsPath := config.PlaylistsPath
	configMutex.RUnlock()

	fullPath := filepath.Join(playlistsPath, req.Playlist)

	// Remove the actual .m3u8 file
	if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
		http.Error(w, "Failed to delete playlist file from disk", http.StatusInternalServerError)
		return
	}

	// Trigger async deletion from Navidrome to keep the UI in sync
	go deletePlaylistFromNavidrome(req.Playlist)

	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// deletePlaylistFromNavidrome interacts with the Subsonic API exposed by Navidrome
// to forcefully remove a playlist from its internal database after it was deleted from the disk.
func deletePlaylistFromNavidrome(playlistFileName string) {
	configMutex.RLock()
	nUrl := config.NaviUrl
	nUser := config.NaviUser
	nPass := config.NaviPass
	configMutex.RUnlock()

	if nUrl == "" || nUser == "" || nPass == "" {
		return // Navidrome integration is not configured
	}

	playlistName := strings.TrimSuffix(playlistFileName, filepath.Ext(playlistFileName))

	// Generate authentication token based on Subsonic API specifications
	salt := "sdlm123"
	hash := md5.Sum([]byte(nPass + salt))
	token := hex.EncodeToString(hash[:])
	
	// Step 1: Fetch all playlists to discover the internal ID of the target playlist
	getURL := fmt.Sprintf("%s/rest/getPlaylists?u=%s&t=%s&s=%s&v=1.16.1&c=SDLMReqHub&f=json", strings.TrimRight(nUrl, "/"), url.QueryEscape(nUser), token, salt)
	
	resp, err := http.Get(getURL)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return
	}

	// Navigate the heavily nested JSON response from the Subsonic API
	root, ok := result["subsonic-response"].(map[string]interface{})
	if !ok { return }
	
	playlistsData, ok := root["playlists"].(map[string]interface{})
	if !ok { return }
	
	playlistList, ok := playlistsData["playlist"].([]interface{})
	if !ok { return }

	var targetId string
	for _, p := range playlistList {
		if pMap, ok := p.(map[string]interface{}); ok {
			if name, ok := pMap["name"].(string); ok && strings.EqualFold(name, playlistName) {
				if id, ok := pMap["id"].(string); ok {
					targetId = id
					break
				}
			}
		}
	}

	if targetId == "" {
		return // Playlist not found in Navidrome's database
	}

	// Step 2: Request deletion using the discovered internal ID
	delURL := fmt.Sprintf("%s/rest/deletePlaylist?id=%s&u=%s&t=%s&s=%s&v=1.16.1&c=SDLMReqHub&f=json", strings.TrimRight(nUrl, "/"), targetId, url.QueryEscape(nUser), token, salt)
	http.Get(delURL)
}

// startNavidromeScan triggers a full media scan in Navidrome.
// This ensures that newly downloaded tracks immediately appear in the Navidrome UI.
func startNavidromeScan() {
	configMutex.RLock()
	nUrl := config.NaviUrl
	nUser := config.NaviUser
	nPass := config.NaviPass
	configMutex.RUnlock()

	if nUrl == "" || nUser == "" || nPass == "" {
		return // Navidrome integration is not configured
	}

	// Generate authentication token
	salt := "sdlm123"
	hash := md5.Sum([]byte(nPass + salt))
	token := hex.EncodeToString(hash[:])
	
	// Trigger the scan endpoint
	scanURL := fmt.Sprintf("%s/rest/startScan?u=%s&t=%s&s=%s&v=1.16.1&c=SDLMReqHub&f=json", strings.TrimRight(nUrl, "/"), url.QueryEscape(nUser), token, salt)
	http.Get(scanURL)
}
