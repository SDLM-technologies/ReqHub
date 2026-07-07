package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

var (
	sseClients      = make(map[chan string]bool)
	sseClientsMutex sync.RWMutex
	broadcastChan   = make(chan string, 10)
)

func init() {
	go handleMessages()
}

func handleMessages() {
	for {
		msg := <-broadcastChan
		sseClientsMutex.RLock()
		for client := range sseClients {
			select {
			case client <- msg:
			default:
			}
		}
		sseClientsMutex.RUnlock()
	}
}

func notifyUpdate() {
	select {
	case broadcastChan <- "UPDATE_NEEDED":
	default:
	}
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Set CORS headers if needed
	w.Header().Set("Access-Control-Allow-Origin", "*")

	clientChan := make(chan string, 5)
	sseClientsMutex.Lock()
	sseClients[clientChan] = true
	sseClientsMutex.Unlock()

	defer func() {
		sseClientsMutex.Lock()
		delete(sseClients, clientChan)
		sseClientsMutex.Unlock()
		close(clientChan)
	}()

	for {
		select {
		case msg := <-clientChan:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}

type SyncAction struct {
	TrackId   string                 `json:"trackId"`
	Action    string                 `json:"action"` // "ADD" or "REMOVE"
	Playlists []string               `json:"playlists,omitempty"`
	Playlist  string                 `json:"playlist,omitempty"`
	Track     map[string]interface{} `json:"track"`
	Line      string                 `json:"line,omitempty"`
}

func handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var actions []SyncAction
	if err := json.NewDecoder(r.Body).Decode(&actions); err != nil {
		http.Error(w, "Invalid body", http.StatusBadRequest)
		return
	}

	for _, action := range actions {
		switch action.Action {
		case "ADD":
			title, _ := action.Track["title"].(string)
			artist, _ := action.Track["artist"].(string)
			album, _ := action.Track["album"].(string)
			req := AddRequest{
				Title:     title,
				Artist:    artist,
				Album:     album,
				Playlists: action.Playlists,
			}
			processAddRequest(req)
		case "REMOVE":
			req := RemoveTrackReq{
				Playlist: action.Playlist,
				Line:     action.Line,
			}
			processRemoveRequest(req)
		}
	}

	notifyUpdate()
	w.WriteHeader(http.StatusOK)
}
