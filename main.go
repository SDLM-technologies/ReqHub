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
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

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
	EnableRsgain  bool   `json:"enableRsgain"`  // Toggle to enable/disable automated rsgain scanning
}

// Global state variables
var config Config
var configMutex sync.RWMutex // Protects concurrent access to the config map

// PendingRequest stores the status and metadata for a requested track.
type PendingRequest struct {
	Playlists     []string `json:"playlists"`
	AddedAt       int64    `json:"addedAt"`
	LastCheckedAt int64    `json:"lastCheckedAt"`
	Status        string   `json:"status"` // "pending", "downloaded"
}

// pendingTracks stores tracks that are requested from Lidarr.
// Key: Lidarr Track ID, Value: PendingRequest struct with tracking info.
var pendingTracks map[int]PendingRequest
var pendingMutex sync.RWMutex // Protects concurrent access to pendingTracks

const dataDir = "data"
var configFile = filepath.Join(dataDir, "config.json")
var pendingFile = filepath.Join(dataDir, "pending_tracks.json")

// --- Initialization ---
// init is called automatically upon application startup.
// It ensures the data directory exists and loads the configuration and pending tracks from disk.
func init() {
	os.MkdirAll(dataDir, 0755)
	pendingTracks = make(map[int]PendingRequest)
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
	// Start background checker for Lidarr requests
	go startPendingChecker()

	// Serve static files and handle root web route
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFile(w, r, "index.html")
		} else if r.URL.Path == "/manifest.json" {
			http.ServeFile(w, r, "manifest.json")
		} else if r.URL.Path == "/service-worker.js" {
			w.Header().Set("Service-Worker-Allowed", "/")
			http.ServeFile(w, r, "service-worker.js")
		} else if strings.HasPrefix(r.URL.Path, "/logo/") {
			http.StripPrefix("/", http.FileServer(http.Dir("."))).ServeHTTP(w, r)
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
	
	// SSE and Sync endpoints
	http.HandleFunc("/api/events", handleEvents)
	http.HandleFunc("/api/sync", handleSync)
	
	// New features
	http.HandleFunc("/api/v1/cleanup", handleCleanup)
	http.HandleFunc("/api/stream", handleStream)
	http.HandleFunc("/api/v1/export", handleExport)
	http.HandleFunc("/api/v1/import", handleImport)
	http.HandleFunc("/api/v1/artist", handleArtistView)
	http.HandleFunc("/api/v1/album", handleAlbumView)
	http.HandleFunc("/api/v1/history", handleGetHistory)
	http.HandleFunc("/api/v1/history/restore", handleRestoreHistory)
	
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

func levenshtein(a, b string) int {
	a = strings.ToLower(a)
	b = strings.ToLower(b)
	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
	}
	for i := range d {
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for j := 1; j <= len(b); j++ {
		for i := 1; i <= len(a); i++ {
			if a[i-1] == b[j-1] {
				d[i][j] = d[i-1][j-1]
			} else {
				min := d[i-1][j]
				if d[i][j-1] < min {
					min = d[i][j-1]
				}
				if d[i-1][j-1] < min {
					min = d[i-1][j-1]
				}
				d[i][j] = min + 1
			}
		}
	}
	return d[len(a)][len(b)]
}

// handleSearch queries iTunes or LRCLIB depending on the mode.
func handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	mode := r.URL.Query().Get("mode")
	if query == "" {
		http.Error(w, "Empty search query", http.StatusBadRequest)
		return
	}

	var tracks []map[string]interface{}

	if mode == "lyrics" {
		lrclibURL := fmt.Sprintf("https://lrclib.net/api/search?q=%s", url.QueryEscape(query))
		resp, err := http.Get(lrclibURL)
		if err == nil {
			defer resp.Body.Close()
			var results []map[string]interface{}
			json.NewDecoder(resp.Body).Decode(&results)
			
			for i, item := range results {
				if i >= 15 { break }
				trackName, _ := item["trackName"].(string)
				artistName, _ := item["artistName"].(string)
				albumName, _ := item["albumName"].(string)
				tracks = append(tracks, map[string]interface{}{
					"title":  trackName,
					"artist": artistName,
					"album":  albumName,
					"cover":  "/logo",
					"source": "LRCLIB",
				})
			}
		}
	} else {
		attribute := ""
		switch mode {
		case "title": attribute = "songTerm"
		case "album": attribute = "albumTerm"
		case "artist": attribute = "artistTerm"
		default: attribute = "mixTerm" 
		}

		itunesURL := fmt.Sprintf("https://itunes.apple.com/search?term=%s&entity=song&limit=25&attribute=%s", url.QueryEscape(query), attribute)
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

		queryClean := strings.ToLower(query)
		for _, item := range result.Results {
			title, _ := item["trackName"].(string)
			artist, _ := item["artistName"].(string)
			album, _ := item["collectionName"].(string)
			cover, _ := item["artworkUrl100"].(string)
			
			if mode != "all" {
				target := ""
				if mode == "title" { target = strings.ToLower(title) }
				if mode == "artist" { target = strings.ToLower(artist) }
				if mode == "album" { target = strings.ToLower(album) }
				
				if target != "" {
					if !strings.Contains(target, queryClean) {
						dist := levenshtein(queryClean, target)
						if dist > len(queryClean)/2 + 2 {
							continue
						}
					}
				}
			}

			tracks = append(tracks, map[string]interface{}{
				"title":  title,
				"artist": artist,
				"album":  album,
				"cover":  cover,
				"source": "iTunes",
			})
			if len(tracks) >= 15 { break }
		}
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

	status, message, err := processAddRequest(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"status": status, "message": message})
}

func processAddRequest(req AddRequest) (string, string, error) {
	searchTerm := fmt.Sprintf("%s %s", req.Artist, req.Album)
	lookupURL := fmt.Sprintf("/api/v1/album/lookup?term=%s", url.QueryEscape(searchTerm))
	var lookupResults []map[string]interface{}
	err := lidarrRequest("GET", lookupURL, nil, &lookupResults)
	if err != nil || len(lookupResults) == 0 {
		return "", "", fmt.Errorf("album not found in Lidarr/MusicBrainz")
	}

	bestAlbum := lookupResults[0]
	
	albumID := 0
	if idVal, ok := bestAlbum["id"].(float64); ok {
		albumID = int(idVal)
	}

	if albumID == 0 {
		bestAlbum["addOptions"] = map[string]interface{}{
			"searchForNewAlbum": false,
		}
		var addedAlbum map[string]interface{}
		err = lidarrRequest("POST", "/api/v1/album", bestAlbum, &addedAlbum)
		if err != nil {
			return "", "", fmt.Errorf("error adding album to Lidarr")
		}
		if idVal, ok := addedAlbum["id"].(float64); ok {
			albumID = int(idVal)
		} else {
			return "", "", fmt.Errorf("Lidarr did not return a valid album ID")
		}
	}

	trackURL := fmt.Sprintf("/api/v1/track?albumId=%d", albumID)
	var tracks []map[string]interface{}
	err = lidarrRequest("GET", trackURL, nil, &tracks)
	if err != nil {
		return "", "", fmt.Errorf("error retrieving tracks from Lidarr")
	}

	var targetTrack map[string]interface{}
	for _, t := range tracks {
		title := t["title"].(string)
		if strings.EqualFold(title, req.Title) || strings.Contains(strings.ToLower(title), strings.ToLower(req.Title)) {
			targetTrack = t
			break
		}
	}

	if targetTrack == nil {
		return "", "", fmt.Errorf("matching track not found in the Lidarr album")
	}

	trackID := int(targetTrack["id"].(float64))
	hasFile := targetTrack["hasFile"].(bool)

	if hasFile {
		trackFileID := int(targetTrack["trackFileId"].(float64))
		
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
				return "", "", fmt.Errorf("error adding/removing playlist: %v", err)
			}
			return "added", "Playlists synchronized successfully!", nil
		}
	}

	pendingMutex.Lock()
	pendingTracks[trackID] = PendingRequest{
		Playlists:     req.Playlists,
		AddedAt:       time.Now().Unix(),
		LastCheckedAt: time.Now().Unix(),
		Status:        "pending",
	}
	pendingMutex.Unlock()
	savePending()

	lidarrRequest("POST", "/api/v1/command", map[string]interface{}{
		"name": "AlbumSearch",
		"albumIds": []int{albumID},
	}, nil)

	return "pending", "Missing track, Lidarr download requested...", nil
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
					if pendingReq, exists := pendingTracks[trackID]; exists && pendingReq.Status == "pending" {
						log.Printf("Pending track imported: %d -> %s\n", trackID, filePath)
						// Add the newly downloaded track to the requested playlists
						syncPlaylists(filePath, pendingReq.Playlists)
						// Update status to downloaded instead of removing
						pendingReq.Status = "downloaded"
						pendingReq.LastCheckedAt = time.Now().Unix()
						pendingTracks[trackID] = pendingReq
					}
				}
			}
			pendingMutex.Unlock()
			savePending()

			// Check if we need to run rsgain on the new album
			configMutex.RLock()
			enableRsgain := config.EnableRsgain
			configMutex.RUnlock()
			
			if enableRsgain {
				albumPath := filepath.Dir(filePath)
				go func(p string) {
					log.Printf("Starting rsgain scan on: %s\n", p)
					// Run the native rsgain binary installed in the Alpine container
					cmd := exec.Command("rsgain", "custom", p)
					out, err := cmd.CombinedOutput()
					if err != nil {
						log.Printf("rsgain error on %s: %v\nOutput: %s\n", p, err, string(out))
					} else {
						log.Printf("rsgain completed successfully for: %s\n", p)
					}
				}(albumPath)
			}

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

	anyModified := false
	filepath.WalkDir(playlistsPath, func(path string, d fs.DirEntry, err error) error {
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
				lines[i] = strings.Replace(line, trimmed, relNewAudioPath, 1)
				modified = true
			}
		}

		if modified {
			out := strings.Join(lines, "\n")
			os.WriteFile(path, []byte(out), 0644)
			anyModified = true
		}

		return nil
	})

	if anyModified {
		notifyUpdate()
	}
}

// syncPlaylists ensures a specific audio file is present ONLY in the playlists specified in `checkedPlaylists`.
// If the track is not present in a requested playlist, it is appended.
// If the track is present in an unchecked playlist, it is removed.
func syncPlaylists(audioPath string, checkedPlaylists []string) error {
	configMutex.RLock()
	playlistsPath := config.PlaylistsPath
	configMutex.RUnlock()

	checkedMap := make(map[string]bool)
	for _, p := range checkedPlaylists {
		checkedMap[p] = true
	}

	anyModified := false
	err := filepath.WalkDir(playlistsPath, func(path string, d fs.DirEntry, err error) error {
		ext := strings.ToLower(filepath.Ext(path))
		if err != nil || d.IsDir() || (ext != ".m3u" && ext != ".m3u8") {
			return nil
		}

		relPlaylistPath, _ := filepath.Rel(playlistsPath, path)
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
		hasExtM3u := false

		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue 
			}
			if strings.HasPrefix(trimmed, "#EXTM3U") {
				hasExtM3u = true
			}
			
			if trimmed == relAudioPath {
				found = true
				if shouldBeInPlaylist {
					newLines = append(newLines, line) 
				} else {
					modified = true 
				}
			} else {
				newLines = append(newLines, line) 
			}
		}

		if shouldBeInPlaylist && !found {
			newLines = append(newLines, relAudioPath) 
			modified = true
		}

		if !hasExtM3u {
			newLines = append([]string{"#EXTM3U"}, newLines...)
			modified = true
		}

		if modified {
			out := strings.Join(newLines, "\n")
			if !strings.HasSuffix(out, "\n") {
				out += "\n"
			}
			os.WriteFile(path, []byte(out), 0644)
			anyModified = true
		}

		return nil
	})

	if anyModified {
		notifyUpdate()
	}
	return err
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

	if err := os.WriteFile(fullPath, []byte("#EXTM3U\n"), 0644); err != nil {
		http.Error(w, "Failed to write playlist file", http.StatusInternalServerError)
		return
	}
	
	notifyUpdate()
	
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

	if err := processRemoveRequest(req); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func processRemoveRequest(req RemoveTrackReq) error {
	if strings.Contains(req.Playlist, "..") {
		return fmt.Errorf("invalid playlist path")
	}

	configMutex.RLock()
	playlistsPath := config.PlaylistsPath
	configMutex.RUnlock()

	fullPath := filepath.Join(playlistsPath, req.Playlist)

	content, err := os.ReadFile(fullPath)
	if err != nil {
		return fmt.Errorf("failed to read playlist file")
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
			go logDeletedTrack(trimmed, req.Playlist)
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
		return fmt.Errorf("failed to update playlist file")
	}
	
	notifyUpdate()
	return nil
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

	if content, err := os.ReadFile(fullPath); err == nil {
		lines := strings.Split(string(content), "\n")
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				go logDeletedTrack(trimmed, req.Playlist)
			}
		}
	}

	if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
		http.Error(w, "Failed to delete playlist file from disk", http.StatusInternalServerError)
		return
	}

	notifyUpdate()

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

// startPendingChecker periodically checks the status of pending Lidarr requests.
// It runs every 24 hours and processes entries that haven't been checked in 7 days.
func startPendingChecker() {
	time.Sleep(5 * time.Minute) // Wait for app to initialize and stabilize
	for {
		pendingMutex.Lock()
		tracksCopy := make(map[int]PendingRequest)
		for k, v := range pendingTracks {
			tracksCopy[k] = v
		}
		pendingMutex.Unlock()

		now := time.Now().Unix()
		changed := false

		for trackID, req := range tracksCopy {
			if now-req.LastCheckedAt < 7*24*3600 {
				continue
			}

			if req.Status == "pending" {
				var trackInfo map[string]interface{}
				err := lidarrRequest("GET", fmt.Sprintf("/api/v1/track/%d", trackID), nil, &trackInfo)
				if err == nil && trackInfo != nil {
					albumIDVal, okAlbum := trackInfo["albumId"].(float64)
					hasFile, okFile := trackInfo["hasFile"].(bool)

					if okAlbum {
						albumID := int(albumIDVal)
						if okFile && hasFile {
							// Webhook might have missed it, but it is downloaded
							trackFileID := int(trackInfo["trackFileId"].(float64))
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
								syncPlaylists(filePath, req.Playlists)
								req.Status = "downloaded"
							}
						} else {
							// Still missing, re-trigger search
							lidarrRequest("POST", "/api/v1/command", map[string]interface{}{
								"name":     "AlbumSearch",
								"albumIds": []int{albumID},
							}, nil)
						}
					}
				}
				req.LastCheckedAt = now
				pendingMutex.Lock()
				pendingTracks[trackID] = req
				pendingMutex.Unlock()
				changed = true

			} else if req.Status == "downloaded" {
				var trackInfo map[string]interface{}
				err := lidarrRequest("GET", fmt.Sprintf("/api/v1/track/%d", trackID), nil, &trackInfo)
				if err == nil && trackInfo != nil {
					if hasFile, ok := trackInfo["hasFile"].(bool); ok && hasFile {
						// Verified after 7 days, remove it from DB
						pendingMutex.Lock()
						delete(pendingTracks, trackID)
						pendingMutex.Unlock()
						changed = true
					} else {
						// File missing? Revert to pending
						req.Status = "pending"
						req.LastCheckedAt = now
						pendingMutex.Lock()
						pendingTracks[trackID] = req
						pendingMutex.Unlock()
						changed = true
					}
				}
			}
		}

		if changed {
			savePending()
		}

		time.Sleep(24 * time.Hour)
	}
}
