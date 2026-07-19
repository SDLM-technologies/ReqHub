### NEW CHANGES
- **PWA Support**: Added `manifest.json` and `service-worker.js` to enable installation as a Progressive Web App and cache static UI assets.
- **Offline Sync (Queue)**: Implemented offline support using `localStorage`. Track additions are queued when offline and automatically pushed to the backend upon reconnection or every 12 hours. Added `/api/sync` to the backend.
- **Server-Sent Events (SSE)**: Added real-time synchronization using `/api/events`. The frontend now automatically pulls playlist updates whenever a server-side modification occurs, preventing UI desynchronization.
- **Conflict Resolution (Flag System)**: The frontend resolves SSE update conflicts by prioritizing offline queued actions (Client Wins).

### UPDATE
- **Robust Lidarr Pending Tracking**: Upgraded the handling of tracks requested but not immediately downloaded by Lidarr.
  - Implemented a `PendingRequest` struct tracking the playlists, addition date, last check date, and current status ("pending", "downloaded").
  - Added a background daemon (`startPendingChecker`) that runs every 24 hours to automatically audit requests older than 7 days.
  - Missing tracks automatically trigger a forced Lidarr `AlbumSearch` command to retry downloading in case of indexer failure.
  - Downloaded tracks are verified one week after download to ensure the file is still successfully retained before being removed from tracking.
- **Streaming (MusicBubble)**: Added a floating audio player that allows streaming any track directly via `yt-dlp`. Added the `/api/stream` backend handler and integrated `yt-dlp` into the Docker build.
- **Disk Cleaner**: Added a manual tool to scan Lidarr's music directory and delete any audio files that are no longer referenced in the active playlists. Added the `/api/v1/cleanup` endpoint and a button in the settings menu.
- **Deleted History Log**: Deleted tracks are now permanently logged in `data/deleted_history.json` for safety auditing.

### UPDATE
- **Search Engine**: Overhauled the search system to support 5 distinct search modes (All, Title, Lyrics, Album, Artist).
- **LRCLIB Integration**: The search backend now securely connects to LRCLIB to provide accurate, direct lyrics-based track searches.
- **Levenshtein Filtering**: Re-wrote iTunes searching to include a strict Levenshtein-distance filter algorithm to drop wildly irrelevant search results from Apple's API when querying for specific modes.
- **Data Portability**: Added robust Global JSON Export and Import functionalities. Users can now export their configuration, all `.m3u` playlists, and deleted tracks history into a single portable `reqhub_export.json` file, and restore it from the UI.
- **Artist & Album Pages**: Added interactive Lidarr-backed pages. Clicking an artist or album from search results loads their full discography, indicating local availability ("Sur le disque" vs "Non possédé").
- **Playlist Enhancements**: Added a "Recommendations" feature using Last.fm API to suggest related tracks for a playlist. Added a toggle to sort playlists (Plus Ancien / Plus Récent). The backend now automatically generates an `All Music.m3u` unified playlist.
- **Membership Manager**: Added a quick-access button to view and manage playlist membership for any track directly from the playlist view.
- **Audio Player Upgrades**: `MusicBubble` now features an interactive progress bar and dark-overlay hover buttons on track covers for quick playback.
- **History UI & Restoration**: Added a full interface to view the `deleted_history.json` ledger, with capabilities to restore deleted tracks individually or in batches via the backend.

### UPDATE
- **Bug Fixes (Backend)**: Fixed a compilation error in `export.go` where `Config` was incorrectly referenced as `AppConfig`. Fixed missing `net/http` import in `history.go` and updated `DeletedTrack` structure to capture comprehensive Lidarr metadata (`ArtistId`, `ForeignReleaseId`, `AlbumTitle`, `DeletedAt`). Fixed `source` tagging for iTunes and LRCLIB in `main.go`. Optimized `sync.go` by replacing if/else logic with a tagged switch statement.
- **Bug Fixes (Frontend)**: Fixed a JavaScript syntax error (`})` missing) on `DOMContentLoaded` and repaired `switchView` UI logic. Added missing detailed comments.
