package main

import (
	"context"
	"embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

//go:embed static
var staticFiles embed.FS

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

var (
	tshockBase    = "http://" + getenv("TSHOCK_HOST", "terraria") + ":" + getenv("TSHOCK_PORT", "7878")
	tshockToken   = getenv("TSHOCK_TOKEN", "")
	containerName = getenv("CONTAINER_NAME", "terraria")
)

func dockerDialer(ctx context.Context, _, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
}

var dockerCtrl = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{DialContext: dockerDialer},
}

var dockerStream = &http.Client{
	Transport: &http.Transport{DialContext: dockerDialer},
}

var tshockHTTP = &http.Client{Timeout: 5 * time.Second}

func main() {
	if tshockToken == "" {
		log.Fatal("TSHOCK_TOKEN env var is required")
	}
	log.Printf("config: container=%s tshock=%s", containerName, tshockBase)

	mux := http.NewServeMux()

	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/start", handleStart)
	mux.HandleFunc("/api/stop", handleStop)
	mux.HandleFunc("/api/restart", handleRestart)
	mux.HandleFunc("/api/logs", handleLogs)
	mux.HandleFunc("/api/players", handlePlayers)
	mux.HandleFunc("/api/tp", handleTP)
	mux.HandleFunc("/api/command", handleCommand)

	log.Println("terraria-web listening on :4823")
	log.Fatal(http.ListenAndServe(":4823", mux))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func dockerAPI(method, path string) (*http.Response, error) {
	req, err := http.NewRequest(method, "http://docker/v1.41"+path, nil)
	if err != nil {
		return nil, err
	}
	return dockerCtrl.Do(req)
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	resp, err := dockerAPI("GET", "/containers/"+containerName+"/json")
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	var info struct {
		State struct {
			Running bool   `json:"Running"`
			Status  string `json:"Status"`
		} `json:"State"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		writeJSON(w, 500, map[string]string{"error": "parse error"})
		return
	}
	writeJSON(w, 200, map[string]any{
		"running": info.State.Running,
		"status":  info.State.Status,
	})
}

func requirePost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	resp, err := dockerAPI("POST", "/containers/"+containerName+"/start")
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	resp.Body.Close()
	writeJSON(w, 200, map[string]string{"result": "started"})
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	resp, err := dockerAPI("POST", "/containers/"+containerName+"/stop")
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	resp.Body.Close()
	writeJSON(w, 200, map[string]string{"result": "stopped"})
}

func handleRestart(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	resp, err := dockerAPI("POST", "/containers/"+containerName+"/restart")
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	resp.Body.Close()
	writeJSON(w, 200, map[string]string{"result": "restarted"})
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	req, err := http.NewRequestWithContext(
		r.Context(), "GET",
		"http://docker/v1.41/containers/"+containerName+"/logs?follow=1&stdout=1&stderr=1&tail=200&timestamps=1",
		nil,
	)
	if err != nil {
		fmt.Fprintf(w, "data: error creating request\n\n")
		flusher.Flush()
		return
	}

	resp, err := dockerStream.Do(req)
	if err != nil {
		fmt.Fprintf(w, "data: docker unavailable\n\n")
		flusher.Flush()
		return
	}
	defer resp.Body.Close()

	// Docker multiplexed stream: 8-byte header (byte 0 = stream type, bytes 4-7 = payload size)
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(resp.Body, hdr); err != nil {
			return
		}
		size := binary.BigEndian.Uint32(hdr[4:8])
		if size == 0 {
			continue
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(resp.Body, payload); err != nil {
			return
		}
		line := strings.TrimRight(string(payload), "\r\n")
		line = strings.ReplaceAll(line, "\n", " ")
		fmt.Fprintf(w, "data: %s\n\n", line)
		flusher.Flush()
	}
}

func tshockDo(path string, params url.Values) (*http.Response, error) {
	params.Set("token", tshockToken)
	req, err := http.NewRequest("GET", tshockBase+path+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	return tshockHTTP.Do(req)
}

func handlePlayers(w http.ResponseWriter, r *http.Request) {
	resp, err := tshockDo("/v2/players/list", url.Values{})
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "tshock unavailable"})
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	io.Copy(w, resp.Body)
}

func handleTP(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	r.ParseForm()
	player := r.FormValue("player")
	destination := r.FormValue("destination")
	if player == "" || destination == "" {
		writeJSON(w, 400, map[string]string{"error": "player and destination required"})
		return
	}
	resp, err := tshockDo("/v3/server/rawcmd", url.Values{
		"cmd": {"/tp " + player + " " + destination},
	})
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "tshock unavailable"})
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	io.Copy(w, resp.Body)
}

func handleCommand(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	r.ParseForm()
	cmd := r.FormValue("cmd")
	if cmd == "" {
		writeJSON(w, 400, map[string]string{"error": "cmd required"})
		return
	}
	// Ensure leading slash for TShock v3 rawcmd
	if cmd[0] != '/' {
		cmd = "/" + cmd
	}
	resp, err := tshockDo("/v3/server/rawcmd", url.Values{"cmd": {cmd}})
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "tshock unavailable"})
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	io.Copy(w, resp.Body)
}
