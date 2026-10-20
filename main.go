package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

type WebhookRequest struct {
	Request struct {
		OriginalUtterance string `json:"original_utterance"`
	} `json:"request"`
	Session struct {
		SessionID string `json:"session_id"`
	} `json:"session"`
}

type WebhookResponse struct {
	Response struct {
		Text       string `json:"text"`
		TTS        string `json:"tts,omitempty"`
		EndSession bool   `json:"end_session"`
	} `json:"response"`
	Version string `json:"version"`
}

// Состояние одной сессии
type SessionState struct {
	mu           sync.Mutex
	history      []openai.ChatCompletionMessageParamUnion
	pendingChan  chan string // Канал, в который горутина положит ответ
	lastAnswer   string      // Архив: последний полученный ответ
	isProcessing bool        // Идет ли сейчас генерация
}

var (
	sessionsMu sync.RWMutex
	sessions   = make(map[string]*SessionState)
)

func getSession(id string) *SessionState {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	if _, ok := sessions[id]; !ok {
		sessions[id] = &SessionState{}
	}
	return sessions[id]
}

func main() {
	port := flag.String("port", "443", "Port to listen on")
	certFile := flag.String("cert", "/etc/letsencrypt/live/45-12-68-191.sslip.io/fullchain.pem", "Path to SSL certificate")
	keyFile := flag.String("key", "/etc/letsencrypt/live/45-12-68-191.sslip.io/privkey.pem", "Path to SSL private key")
	flag.Parse()

	amneziaURL, _ := url.Parse("http://127.0.0.1:8443")
	proxy := httputil.NewSingleHostReverseProxy(amneziaURL)

	http.HandleFunc("/webhook", handleWebhook)
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

	text := strings.ToLower(strings.TrimSpace(req.Request.OriginalUtterance))
	sessionID := req.Session.SessionID
	sess := getSession(sessionID)

	sess.mu.Lock()
	defer sess.mu.Unlock()

	// 1. Обработка "повтори"
	if strings.Contains(text, "повтори") {
		if sess.lastAnswer != "" {
			sendResponse(w, "Повторяю: "+sess.lastAnswer)
		} else {
			sendResponse(w, "Мне пока нечего повторять.")
		}
		return
	}

	// 2. Обработка "ты подумал?"
	if strings.Contains(text, "подумал") || strings.Contains(text, "готов") {
		if sess.lastAnswer != "" {
			// Ответ уже готов (горутина успела)
			ans := sess.lastAnswer
			sess.lastAnswer = "" // Очищаем архив после выдачи
			sendResponse(w, ans)
			return
		} else if sess.isProcessing {
			// Ответа пока нет, но генерация идет. Ждем до 3.5 секунд.
			select {
			case ans := <-sess.pendingChan:
				sess.isProcessing = false
				sess.lastAnswer = "" // Сразу отдаем, не сохраняем
				sendResponse(w, ans)
			case <-time.After(3500 * time.Millisecond):
				// Не дождались
				sendResponse(w, "Я всё ещё думаю. Спроси чуть позже.")
			}
			return
		} else {
			sendResponse(w, "Я не думаю над чем-то сейчас.")
			return
		}
	}

	// Если текст пустой
	if text == "" {
		sendResponse(w, "")
		return
	}

	// 3. Обычный вопрос
	// Добавляем в историю
	sess.history = append(sess.history, openai.UserMessage(text))
	if len(sess.history) > 10 {
		sess.history = sess.history[len(sess.history)-10:]
	}

	// Создаем канал для фоновой задачи, если его нет
	if sess.pendingChan == nil {
		sess.pendingChan = make(chan string, 1)
	}

	// Запускаем горутину для запроса к LLM
	sess.isProcessing = true
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second) // Даем 15 секунд на раздумья
		defer cancel()

		apiKey := os.Getenv("ZAI_API_KEY")
		client := openai.NewClient(option.WithAPIKey(apiKey), option.WithBaseURL("https://api.z.ai/api/paas/v4/"))

		messages := []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage("Отвечай максимально кратко. Твой ответ должен уместиться в 1024 символа."),
		}
		messages = append(messages, sess.history...)

		resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Messages: messages,
			Model:    "glm-4.5-flash",
		}, option.WithJSONSet("thinking", map[string]string{"type": "disabled"}))

		sess.mu.Lock()
		defer sess.mu.Unlock()
		sess.isProcessing = false

		if err != nil {
			log.Printf("zai error in goroutine: %v", err)
			sess.pendingChan <- "Произошла ошибка при генерации."
			return
		}

		ans := resp.Choices[0].Message.Content
		sess.history = append(sess.history, openai.AssistantMessage(ans))

		// Отправляем в канал
		select {
		case sess.pendingChan <- ans:
		default:
			// Если канал переполнен (никто не слушает), сохраняем в архив
		}
		sess.lastAnswer = ans
	}()

	// Ждем 3.5 секунды синхронно (чтобы уложиться в лимит Яндекса)
	select {
	case ans := <-sess.pendingChan:
		sess.isProcessing = false
		sendResponse(w, ans)
	case <-time.After(4100 * time.Millisecond):
		// Таймаут
		sendResponse(w, "Мне надо подумать над вопросом. Спроси 'ты подумал?' чуть позже.")
	}
}

func sendResponse(w http.ResponseWriter, text string) {
	var resp WebhookResponse
	resp.Response.Text = text
	resp.Response.TTS = text
	resp.Response.EndSession = false
	resp.Version = "1.0"
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
