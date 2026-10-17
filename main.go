package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

type WebhookRequest struct {
	Request struct {
		OriginalUtterance string `json:"original_utterance"`
	} `json:"request"`
}

type WebhookResponse struct {
	Response struct {
		Text       string `json:"text"`
		TTS        string `json:"tts,omitempty"`
		EndSession bool   `json:"end_session"`
	} `json:"response"`
	Version string `json:"version"`
}

func main() {
	port := flag.String("port", "443", "Port to listen on")
	certFile := flag.String("cert", "/etc/letsencrypt/live/45-12-68-191.sslip.io/fullchain.pem", "Path to SSL certificate")
	keyFile := flag.String("key", "/etc/letsencrypt/live/45-12-68-191.sslip.io/privkey.pem", "Path to SSL private key")
	flag.Parse()

	// Куда перенаправлять VPN-трафик (Amnezia)
	amneziaURL, _ := url.Parse("http://127.0.0.1:8443")
	proxy := httputil.NewSingleHostReverseProxy(amneziaURL)

	http.HandleFunc("/webhook", handleWebhook)

	// Всё, что не /webhook — отправляем в Amnezia
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	})

	addr := ":" + *port
	log.Printf("Starting HTTPS server on %s", addr)

	err := http.ListenAndServeTLS(addr, *certFile, *keyFile, nil)
	if err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req WebhookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	text := req.Request.OriginalUtterance
	log.Printf("Alice says: %s", text)

	var resp WebhookResponse
	resp.Response.Text = text
	resp.Response.TTS = text
	resp.Response.EndSession = false
	resp.Version = "1.0"

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
