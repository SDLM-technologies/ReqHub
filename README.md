# SDLM ReqHub 🎵

**SDLM ReqHub** is an ultra-lightweight web application designed specifically to bridge the gap between your music searches, your **Lidarr** download manager, and your streaming server (e.g., **Navidrome**).

The application allows you to search for a specific track, trigger its download via Lidarr if it is missing, and automatically and dynamically add it to your local (`.m3u`) playlists once the file is physically present on your disk.

## 🌟 Features

- **Instant Search:** Uses the public iTunes API for precise track and album searches (no account or API key required).
- **Smart Logic & Two-Way Synchronization:** 
  - **Pre-loading:** When selecting a track, the app instantly checks if it already exists on your disk. If it does, it reads all your `.m3u` files and **automatically pre-checks** the playlists where the track is already present.
  - **Instant Addition:** If the music exists but is not in the selected playlist, it is added to the `.m3u` instantly.
  - **Easy Removal:** If a track was already in a playlist but you **uncheck** it, the app cleanly removes the corresponding line from the `.m3u` file.
  - **Missing Track Scenario:** If the music is missing, the app instructs Lidarr to search for and download the album.
- **Automated Addition (Webhook):** As soon as Lidarr finishes downloading, ReqHub captures the file and injects it into the playlists you previously checked.
- **Built-in Playlist Manager:** Dedicated interface to create, read, modify (add/remove tracks via UI), and delete your local `.m3u` playlists.
- **Ultra Lightweight:** Backend written in **pure Go** (no external dependencies, `net/http` only). Frontend in **native HTML/CSS/JS**.
- **Dark Mode:** Elegant interface featuring enterprise colors (Purple and Yellow).
- **NAS Optimized (TrueNAS, Unraid, etc.):** CPU/RAM consumption close to zero, multi-stage Docker build resulting in a tiny Alpine/Scratch final image.

---

## 🤖 About this Code (Disclaimer)

This project was built using an AI assistant (it was **"vibe coded"**). While the core architecture, logic, and features were mainly conceived and designed by me, I do not personally know the specific nomenclature or syntax of Go, HTML, JS, or CSS. 

Therefore, if you are a developer and you spot any non-standard practices, bugs, or errors in the code, please do not hesitate to open a Pull Request or an Issue. Contributions and corrections are more than welcome!

---

## 🚀 Deployment Guide

You can deploy SDLM ReqHub easily using Docker Compose.

### Step 1: docker-compose.yml

Create a `docker-compose.yml` file with the following content (or copy the one from this repository):

```yaml
version: '3.8'

services:
  reqhub:
    image: ghcr.io/sdlm-technologies/reqhub:latest
    container_name: sdlm-reqhub
    ports:
      - "8080:8080"
    environment:
      - TZ=Europe/Paris
    volumes:
      # /!\ The left path AND the right path MUST BE IDENTICAL.
      # /!\ E.g. - /mnt/pool/music:/mnt/pool/music:rw
      # /!\ This path must exactly match the path that Lidarr knows and uses.
      - /mnt/your_pool/music_folder:/mnt/your_pool/music_folder:rw
      
      # Local folder to save the configuration
      - ./data:/app/data
    restart: unless-stopped
```

### Step 2: Start the container

**Option A: Via Dockge / Portainer (Recommended)**
1. Open your Dockge or Portainer interface.
2. Create a new Stack/Compose.
3. Paste the `docker-compose.yml` content and adapt your volume paths and the image registry URL.
4. Deploy!

**Option B: Via SSH (Classic)**
1. Create a folder on your server (e.g., `/opt/reqhub`).
2. Place the modified `docker-compose.yml` in it.
3. Run the deployment:
   ```bash
   docker-compose up -d
   ```

### Step 3: SDLM ReqHub Web Configuration
1. Open your web browser and go to `http://<YOUR_SERVER_IP>:8080`.
2. Click on the **Settings (⚙️)** icon in the top right corner.
3. Fill in the form:
   - **Lidarr URL:** The complete address of your Lidarr instance (e.g., `http://192.168.1.50:8686`).
   - **Lidarr API Key:** Retrieve this from your Lidarr interface (*Settings > General > Security > API Key*).
   - **NAS Music Folder (Absolute):** The absolute path to your music folder (e.g. `/mnt/your_pool/music_folder`). This must perfectly match the volume mapping in your `docker-compose.yml`.
4. Click **Save**.

### Step 4: Lidarr Webhook Configuration
For ReqHub to be notified when a download is finished, you must configure Lidarr:
1. Go to your **Lidarr** web interface.
2. Go to **Settings** > **Connect**.
3. Click the **+** icon to add a connection and choose **Webhook**.
4. Configure it as follows:
   - **Name:** `SDLM ReqHub`
   - **On Track Import:** ✅ Check this box (this is mandatory!).
   - **URL:** `http://<YOUR_SERVER_IP>:8080/webhook` (The IP where ReqHub is installed).
   - **Method:** `POST`
5. Click the **Test** button. You should see a green checkmark proving the communication works.
6. Click **Save**.

---

## 🎉 Ready to go!

1. Type a track name in the SDLM ReqHub search bar.
2. Check the playlists (`.m3u`) you want to add it to.
3. Click **Add**.

The application will automatically handle everything else!

## 📂 File Architecture
- `main.go` : Backend source code (Go).
- `index.html` : Single-page user interface (HTML/JS/CSS).
- `Dockerfile` : Recipe to build the lightweight container.
- `docker-compose.yml` : Docker deployment configuration.
- `.github/workflows/` : CI/CD pipeline to automatically build and publish the Docker image to GHCR.
- `data/` : Automatically created folder containing your configuration (`config.json`) and pending tracks (`pending_tracks.json`).


