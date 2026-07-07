package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"
)

type ArtistViewData struct {
	Name   string                   `json:"name"`
	Image  string                   `json:"image"`
	Albums []map[string]interface{} `json:"albums"`
}

func handleArtistView(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "Missing name", http.StatusBadRequest)
		return
	}

	var lookup []map[string]interface{}
	err := lidarrRequest("GET", "/api/v1/artist/lookup?term="+url.QueryEscape(name), nil, &lookup)
	if err != nil || len(lookup) == 0 {
		http.Error(w, "Artist not found", http.StatusNotFound)
		return
	}

	artist := lookup[0]
	artistId := int(artist["id"].(float64))
	
	image := "/logo"
	if images, ok := artist["images"].([]interface{}); ok && len(images) > 0 {
		if img, ok := images[0].(map[string]interface{}); ok {
			image = img["url"].(string)
		}
	}

	var albums []map[string]interface{}

	if artistId != 0 {
		var dbAlbums []map[string]interface{}
		lidarrRequest("GET", fmt.Sprintf("/api/v1/album?artistId=%d", artistId), nil, &dbAlbums)
		
		for _, al := range dbAlbums {
			hasFile := false
			if stats, ok := al["statistics"].(map[string]interface{}); ok {
				if percent, ok := stats["percentOfTracks"].(float64); ok && percent == 100 {
					hasFile = true
				} else if size, ok := stats["sizeOnDisk"].(float64); ok && size > 0 {
					hasFile = true
				}
			}
			
			cover := "/logo"
			if images, ok := al["images"].([]interface{}); ok && len(images) > 0 {
				if img, ok := images[0].(map[string]interface{}); ok {
					cover = img["url"].(string)
				}
			}

			releaseDate, _ := al["releaseDate"].(string)

			albums = append(albums, map[string]interface{}{
				"id":          al["id"],
				"foreignId":   al["foreignAlbumId"],
				"title":       al["title"],
				"releaseDate": releaseDate,
				"cover":       cover,
				"hasFile":     hasFile,
			})
		}
	} else {
		itunesURL := fmt.Sprintf("https://itunes.apple.com/search?term=%s&entity=album&limit=50", url.QueryEscape(name))
		resp, err := http.Get(itunesURL)
		if err == nil {
			defer resp.Body.Close()
			var result struct {
				Results []map[string]interface{} `json:"results"`
			}
			json.NewDecoder(resp.Body).Decode(&result)
			for _, al := range result.Results {
				albums = append(albums, map[string]interface{}{
					"id":          0,
					"foreignId":   fmt.Sprintf("%v", al["collectionId"]),
					"title":       al["collectionName"],
					"releaseDate": al["releaseDate"],
					"cover":       al["artworkUrl100"],
					"hasFile":     false,
				})
			}
		}
	}

	sort.Slice(albums, func(i, j int) bool {
		d1, _ := albums[i]["releaseDate"].(string)
		d2, _ := albums[j]["releaseDate"].(string)
		t1, _ := time.Parse(time.RFC3339, d1)
		t2, _ := time.Parse(time.RFC3339, d2)
		return t1.After(t2)
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ArtistViewData{
		Name:   name,
		Image:  image,
		Albums: albums,
	})
}

type AlbumViewData struct {
	Title   string                   `json:"title"`
	Artist  string                   `json:"artist"`
	Cover   string                   `json:"cover"`
	HasFile bool                     `json:"hasFile"`
	Tracks  []map[string]interface{} `json:"tracks"`
}

func handleAlbumView(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	foreignId := r.URL.Query().Get("foreignId")
	artistName := r.URL.Query().Get("artist")

	if idStr == "" && foreignId == "" {
		http.Error(w, "Missing id", http.StatusBadRequest)
		return
	}

	data := AlbumViewData{
		Artist: artistName,
		Tracks: []map[string]interface{}{},
	}

	if idStr != "" && idStr != "0" {
		var album map[string]interface{}
		lidarrRequest("GET", "/api/v1/album/"+idStr, nil, &album)
		
		if title, ok := album["title"].(string); ok {
			data.Title = title
		}
		data.Cover = "/logo"
		if images, ok := album["images"].([]interface{}); ok && len(images) > 0 {
			if img, ok := images[0].(map[string]interface{}); ok {
				data.Cover = img["url"].(string)
			}
		}
		if stats, ok := album["statistics"].(map[string]interface{}); ok {
			if percent, ok := stats["percentOfTracks"].(float64); ok && percent == 100 {
				data.HasFile = true
			} else if size, ok := stats["sizeOnDisk"].(float64); ok && size > 0 {
				data.HasFile = true
			}
		}

		var tracks []map[string]interface{}
		lidarrRequest("GET", "/api/v1/track?albumId="+idStr, nil, &tracks)
		for _, tr := range tracks {
			data.Tracks = append(data.Tracks, map[string]interface{}{
				"title": tr["title"],
				"artist": artistName,
				"album": data.Title,
				"cover": data.Cover,
				"duration": tr["duration"],
			})
		}
	} else if foreignId != "" {
		itunesURL := fmt.Sprintf("https://itunes.apple.com/lookup?id=%s&entity=song", url.QueryEscape(foreignId))
		resp, err := http.Get(itunesURL)
		if err == nil {
			defer resp.Body.Close()
			var result struct {
				Results []map[string]interface{} `json:"results"`
			}
			json.NewDecoder(resp.Body).Decode(&result)
			for i, item := range result.Results {
				if i == 0 {
					data.Title, _ = item["collectionName"].(string)
					data.Cover, _ = item["artworkUrl100"].(string)
					if data.Artist == "" {
						data.Artist, _ = item["artistName"].(string)
					}
					data.HasFile = false
				} else {
					data.Tracks = append(data.Tracks, map[string]interface{}{
						"title": item["trackName"],
						"artist": data.Artist,
						"album": data.Title,
						"cover": data.Cover,
					})
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}
