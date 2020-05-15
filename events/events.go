package events

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-redis/redis/v7"
)

type Handler struct {
	redis *redis.Client
}

func New(redis *redis.Client) *Handler {
	return &Handler{
		redis: redis,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	channels := strings.Split(r.FormValue("channels"), ",")
	pubsub := h.redis.PSubscribe(channels...)
	defer pubsub.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	_, _ = w.Write([]byte(": hello\n\n"))
	w.(http.Flusher).Flush()

	const pingTime = 45 * time.Second
	pingChannel := time.After(pingTime)
	for {
		output := ""
		select {
		case message := <-pubsub.Channel():
			output = fmt.Sprintf("data: %s\n\n", message.Payload)
		case <-pingChannel:
			pingChannel = time.After(pingTime)
			output = ": ping\n\n"
		}

		_, err := w.Write([]byte(output))
		if err == nil {
			w.(http.Flusher).Flush()
		} else {
			log.Printf("write failed, dropping connection: %v", err)
			break
		}
	}
}
