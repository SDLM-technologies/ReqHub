# ReqHub Technical Documentation

This document explains the internal architecture, algorithms, and technical implementations of every feature inside SDLM Request Hub (ReqHub). It serves to satisfy the requirement for deep technical details and under-the-hood operations.

## Architecture Overview
ReqHub is a monolithic application designed to minimize dependencies and run in constrained environments (Alpine/Scratch Docker images).
- **Backend**: Written in `Go` using the native `net/http` library. Concurrency and data safety are managed exclusively through `sync.RWMutex` to avoid external database dependencies like SQLite or Redis.
- **Frontend**: A pure vanilla HTML, CSS, and JavaScript Single Page Application (SPA). The UI operates by toggling DOM element visibility via CSS classes (`.active`).
- **State Management**: Persisted strictly through standard JSON files (`data/config.json`, `data/pending_tracks.json`, `data/deleted_history.json`).

## Feature: Offline PWA & Sync Engine
### Under the Hood
1. **PWA**: The application is served alongside a `manifest.json` and a `service-worker.js`. The Service Worker caches `index.html` and assets on the first visit. If the backend goes down, the SW intercepts HTTP requests and serves the cached interface.
2. **Offline Queuing**: When a user clicks "Add" on a track and `navigator.onLine` is false, the JavaScript serializes the `AddRequest` and pushes it to an array in `localStorage` under the key `offlineQueue`.
3. **Reconciliation**: A listener on the `window` object for the `online` event triggers `syncOfflineQueue()`. This function reads the array, sends it as a batch POST request to `/api/sync`, and flushes `localStorage` upon receiving a `200 OK`. 

## Feature: Server-Sent Events (SSE)
### Under the Hood
ReqHub employs a unidirectional event stream to keep connected clients synchronized.
1. **Go Broadcaster**: The backend maintains a `map[chan string]bool` representing all connected clients. When an event occurs (e.g., a file is modified), `notifyUpdate()` sends an `UPDATE_NEEDED` string to all channels in the map.
2. **HTTP Handler**: The `/api/events` endpoint sets headers to `text/event-stream` and `Connection: keep-alive`. It blocks inside a `for` loop, waiting to receive messages from its assigned channel, writing them to the `http.ResponseWriter` and calling `w.(http.Flusher).Flush()`.
3. **Conflict Resolution**: If the frontend receives an `UPDATE_NEEDED` ping but currently has unsynchronized offline tasks in `localStorage`, it deliberately ignores the SSE refresh to prevent the user's offline queue from being overwritten (Client Wins strategy). Otherwise, it triggers `loadPlaylists()`.

## Feature: Search Engine & API Aggregation
### Under the Hood
The `handleSearch` endpoint acts as a multiplexer:
- **Lidarr & iTunes**: For standard searches, it first attempts to query the iTunes API. It constructs query parameters based on the mode (`songTerm`, `albumTerm`, `artistTerm`).
- **Levenshtein Filtering**: To mitigate Apple's notoriously loose search matching, the Go backend implements the Levenshtein distance algorithm. If a search is targeting an "Artist" and the distance between the query and the returned result is greater than `len(query)/2 + 2`, the result is aggressively discarded.
- **LRCLIB**: If the mode is `lyrics`, the backend proxies the request to `lrclib.net`, mapping their `trackName` fields into ReqHub's standardized track payload.
- **Last.fm Recommendations**: The frontend uses `ws.audioscrobbler.com/2.0` with a seeded track from the current playlist, retrieving `similartracks` and directly mapping them into playable `track-card` elements.

## Feature: MusicBubble Audio Player
### Under the Hood
- **Remote Streaming**: Standard HTML5 `<audio>` players cannot reliably stream media located on NAS mount paths outside of the web server's scope, nor do they support arbitrary remote lidarr links easily. 
- **The Wrapper**: ReqHub intercepts playback via `playTrack()`, hitting `/api/stream`. The Go backend receives the title/artist and invokes the `yt-dlp` binary natively compiled in the Docker container.
- **Standard Output Piping**: Go uses `os/exec` to run `yt-dlp` using the "ytsearch:" prefix. `cmd.StdoutPipe()` is attached directly to the `http.ResponseWriter` via `io.Copy`. The browser receives the output as a continuous `audio/mpeg` stream.

## Feature: Disk Cleaner & Safety Net
### Under the Hood
1. **Scanning Algorithm**: The `/api/v1/cleanup` endpoint recursively walks `config.PlaylistsPath` and parses every `.m3u` file, building a giant `map[string]bool` in memory of every referenced audio file.
2. **Orphan Detection**: It then queries Lidarr (`/api/v1/trackfile`) for every single track file physically present on the disk. Any track file returned by Lidarr that is *not* present in the Go map is considered an orphan.
3. **Deletion Logging**: Before issuing the DELETE command to Lidarr, the `deleted_history.json` is appended with the `ForeignReleaseId` (MusicBrainz ID) and `AlbumTitle`.
4. **Restoration**: The UI provides a "Historique de Corbeille" view. Clicking "Restaurer" sends the `ForeignReleaseId` back to Lidarr via `/api/v1/album`, triggering an automated search and re-download.

## Feature: Data Portability
### Under the Hood
ReqHub guarantees zero vendor lock-in. 
- **Export**: When `/api/v1/export` is hit, the backend serializes `config.json`, the full `deleted_history.json`, and recursively reads every `.m3u` file into a massive string map. This map is zipped into a single JSON payload (`reqhub_export.json`) and served with a `Content-Disposition: attachment` header.
- **Import**: Users upload the `reqhub_export.json`. The backend deserializes it in memory, atomically replaces the config and history arrays, and forcefully writes the string arrays back to the disk as literal `.m3u` files, instantly restoring the exact state of the library.

## Feature: Automatic Master Playlist (All Music.m3u)
### Under the Hood
Whenever `indexPlaylists()` is called (during load or after any playlist modification), the Go backend iterates over every parsed M3U array. It maintains a `map[string]bool` of unique `#EXTINF` lines. Any line not already in the map is appended. The resulting aggregate array is immediately written to disk as `All Music.m3u`, effectively creating a completely automated "All Tracks" Smart Playlist without requiring SQL triggers.
