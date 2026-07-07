package main

import (
	"fmt"
	"net/http"
	"os/exec"
)

// handleStream streams audio directly to the client using yt-dlp.
func handleStream(w http.ResponseWriter, r *http.Request) {
	title := r.URL.Query().Get("title")
	artist := r.URL.Query().Get("artist")

	if title == "" || artist == "" {
		http.Error(w, "Missing title or artist", http.StatusBadRequest)
		return
	}

	searchQuery := fmt.Sprintf("ytsearch1:%s %s audio", artist, title)

	// Stream audio directly to stdout using yt-dlp
	cmd := exec.Command("yt-dlp", "-f", "bestaudio", "-q", "-o", "-", searchQuery)
	
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Transfer-Encoding", "chunked")

	cmd.Stdout = w

	if err := cmd.Run(); err != nil {
		fmt.Printf("Error streaming via yt-dlp: %v\n", err)
	}
}
