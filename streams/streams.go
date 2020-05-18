package streams

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strconv"

	"github.com/go-redis/redis/v7"
	"github.com/gorilla/mux"

	"github.com/PonyFest/music-control/songs"
)

const upNextFormat = "upnext-%s"
const recentlyPlayedFormat = "recent-%s"
const stateFormat = "state-%s"
const eventsFormat = "events-%s"

type Handler struct {
	mux   *mux.Router
	redis *redis.Client
	root  string
}

func New(redisClient *redis.Client, rootURL string) *Handler {
	h := &Handler{
		mux:   mux.NewRouter(),
		redis: redisClient,
		root:  rootURL,
	}
	h.mux.HandleFunc("/{stream}/next", h.handleNext)
	h.mux.HandleFunc("/{stream}/upnext", h.handleUpNext)
	h.mux.HandleFunc("/{stream}/state", h.handleState)
	return h
}

func (h *Handler) handleUpNext(w http.ResponseWriter, r *http.Request) {
	stream := mux.Vars(r)["stream"]
	switch r.Method {
	case http.MethodGet:
		result := h.redis.LRange(fmt.Sprintf(upNextFormat, stream), 0, -1).Val()
		// nil results in JSON output are annoying; force an empty list.
		if result == nil {
			result = []string{}
		}
		if err := json.NewEncoder(w).Encode(map[string]interface{}{"upNext": result, "status": "ok"}); err != nil {
			http.Error(w, fmt.Sprintf("encoding json somehow failed: %v", err), http.StatusInternalServerError)
			return
		}
	case http.MethodPut:
		trackId := r.FormValue("trackId")
		if h.redis.Exists(trackId).Val() == 0 {
			http.Error(w, fmt.Sprintf("no such track %q", trackId), http.StatusFailedDependency)
			return
		}
		if err := h.redis.RPush(fmt.Sprintf(upNextFormat, stream), trackId).Err(); err != nil {
			http.Error(w, fmt.Sprintf("pushing track failed: %v", err), http.StatusInternalServerError)
			return
		}
		h.publishUpNextUpdate(stream)
		_, _ = w.Write([]byte(`{"status": "ok"}`))
	case http.MethodDelete:
		// instead of actually deleting things, we tombstone them to avoid index confusion.
		// index confusion can still occur at the end of a track, when we will be popping things from the head
		// of the list, which we could mitigate if we checked that the thing being removed is actually the thing
		// we think we should be removing (and giving up if it isn't, I guess?)
		indexString := r.FormValue("index")
		index, err := strconv.ParseInt(indexString, 10, 32)
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid track index %q: %v", indexString, err), http.StatusBadRequest)
			return
		}
		if err := h.redis.LSet(fmt.Sprintf(upNextFormat, stream), index, "").Err(); err != nil {
			http.Error(w, fmt.Sprintf("failed to remove up next entry at index %d: %v", index, err), http.StatusBadRequest)
			return
		}
		h.publishUpNextUpdate(stream)
		_, _ = w.Write([]byte(`{"status": "ok"}`))
	}
}

func (h *Handler) handleNext(w http.ResponseWriter, r *http.Request) {
	stream := mux.Vars(r)["stream"]
	for {
		next, err := h.redis.LPop(fmt.Sprintf(upNextFormat, stream)).Result()
		if err == redis.Nil {
			break
		}
		if next == "" {
			continue
		}
		if h.redis.Exists(next).Val() == 0 {
			continue
		}
		trackData, err := h.trackIdToTrack(next)
		if err != nil {
			http.Error(w, fmt.Sprintf("looking up extant track failed I guess: %v", err), http.StatusInternalServerError)
			return
		}
		h.publishUpNextUpdate(stream)
		if err := json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "track": trackData}); err != nil {
			http.Error(w, fmt.Sprintf("encoding JSON failed: %v", err), http.StatusInternalServerError)
			return
		}
		return
	}

	// If we get here then it means we didn't find anything useful in the up next list, so we need to select
	// some random track.
	// In this case, we should pick a track that isn't too recently played.
	// since we expect these lists to be fairly small, we just fetch the entire library and the recently played list,
	// subtract the latter from the former, and then pick a random entry.
	p := h.redis.Pipeline()
	recentlyPlayed := p.LRange(fmt.Sprintf(recentlyPlayedFormat, stream), 0, -1)
	allTracks := p.SMembers(songs.TrackPoolKey)
	if _, err := p.Exec(); err != nil {
		http.Error(w, fmt.Sprintf("looking up track collections failed: %v", err), http.StatusInternalServerError)
		return
	}
	availableTracks := map[string]struct{}{}
	for _, trackId := range allTracks.Val() {
		availableTracks[trackId] = struct{}{}
	}
	for _, trackId := range recentlyPlayed.Val() {
		delete(availableTracks, trackId)
	}
	// If we are left with no candidates, and we have ever played anything, play the least-most-recently played track
	// If we have no options and we have never played anything, presumably there is no music - give up.
	if len(availableTracks) == 0 {
		if len(recentlyPlayed.Val()) == 0 {
			http.Error(w, "apparently there is no music to play", http.StatusTeapot)
			return
		}
		oldestTrack := recentlyPlayed.Val()[len(recentlyPlayed.Val())-1]
		trackData, err := h.trackIdToTrack(oldestTrack)
		if err != nil {
			http.Error(w, fmt.Sprintf("found the oldest track but also didn't: %v", err), http.StatusInternalServerError)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "track": trackData}); err != nil {
			http.Error(w, fmt.Sprintf("encoding JSON failed: %v", err), http.StatusInternalServerError)
			return
		}
		return
	}
	selectionList := make([]string, 0, len(availableTracks))
	for track := range availableTracks {
		selectionList = append(selectionList, track)
	}
	track := selectionList[rand.Intn(len(selectionList))]
	// look up the track and include that metadata
	trackData, err := h.trackIdToTrack(track)
	if err != nil {
		http.Error(w, fmt.Sprintf("found a track but also didn't: %v", err), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "track": trackData}); err != nil {
		http.Error(w, fmt.Sprintf("encoding JSON failed: %v", err), http.StatusInternalServerError)
		return
	}
}

func (h *Handler) trackIdToTrack(trackId string) (map[string]string, error) {
	track, err := h.redis.HGetAll(trackId).Result()
	if err != nil {
		return nil, fmt.Errorf("couldn't look up track: %v", err)
	}
	track["trackId"] = trackId
	track["trackUrl"] = h.trackIdToURL(trackId)
	return track, nil
}

func (h *Handler) publishUpNextUpdate(stream string) {
	upNext := h.redis.LRange(fmt.Sprintf(upNextFormat, stream), 0, -1).Val()
	j, err := json.Marshal(map[string]interface{}{
		"event":  "updateUpNext",
		"stream": stream,
		"upNext": upNext,
	})
	if err != nil {
		log.Printf("Failed to marshal json: %v.\n", err)
		return
	}
	if err := h.redis.Publish(fmt.Sprintf(eventsFormat, stream), j).Err(); err != nil {
		log.Printf("Failed to publish up next update: %v.\n", err)
		return
	}
}

func (h *Handler) handleState(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, fmt.Sprintf("parsing form failed: %v", err), http.StatusBadRequest)
		return
	}
	stream := mux.Vars(r)["stream"]
	stateKey := fmt.Sprintf(stateFormat, stream)
	switch r.Method {
	case http.MethodPatch:
		for k, sv := range r.Form {
			if len(sv) == 0 {
				continue
			}
			v := sv[0]
			switch k {
			case "currentTrack":
				p := h.redis.Pipeline()
				p.HSet(stateKey, "currentTrack", v)
				// Remove the current entry in the recently played list, if any
				// This produces saner behaviour if the list is larger than the track pool.
				p.LRem(fmt.Sprintf(recentlyPlayedFormat, stream), 0, v)
				// Make this the most recent played
				p.LPush(fmt.Sprintf(recentlyPlayedFormat, stream), v)
				// Truncate the list
				p.LTrim(fmt.Sprintf(recentlyPlayedFormat, stream), 0, 19)
				results, err := p.Exec()
				if err != nil {
					http.Error(w, fmt.Sprintf("failed to execute current track update: %v", err), http.StatusInternalServerError)
					break
				}
				for _, result := range results {
					if result.Err() != nil {
						http.Error(w, fmt.Sprintf("failed to execute current track update: %v", result.Err()), http.StatusInternalServerError)
						break
					}
				}
				if err := h.publishUpdate(stream, k, v); err != nil {
					log.Printf("Failed to publish update: %v.\n", err)
				}
			case "playing":
				fallthrough
			case "autoplay":
				if err := h.redis.HSet(stateKey, k, v).Err(); err != nil {
					log.Printf("Failed to update %q state: %v.\n", k, err)
				}
				if err := h.publishUpdate(stream, k, v); err != nil {
					log.Printf("Failed to publish update: %v.\n", err)
				}
			case "skip":
				j, err := json.Marshal(map[string]string{
					"event":  "requestSkip",
					"stream": stream,
				})
				if err != nil {
					log.Printf("Failed to marshal json: %v.\n", err)
					continue
				}
				if err := h.redis.Publish(fmt.Sprintf(eventsFormat, stream), j).Err(); err != nil {
					log.Printf("Failed to publish skip request: %v.\n", err)
					continue
				}
			}
		}
	case http.MethodGet:
		state, err := h.redis.HGetAll(stateKey).Result()
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to fetch information: %v", err), http.StatusInternalServerError)
			return
		}
		result := map[string]interface{}{}
		for k, v := range state {
			result[k] = v
		}
		if trackId, ok := state["currentTrack"]; ok {
			track, err := h.redis.HGetAll(trackId).Result()
			if err == nil {
				track["trackId"] = trackId
				track["trackUrl"] = h.trackIdToURL(trackId)
				result["currentTrack"] = track
			} else {
				delete(result, "currentTrack")
			}
		}
		if err := json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "state": result}); err != nil {
			http.Error(w, fmt.Sprintf("failed to marshal json: %v", err), http.StatusInternalServerError)
			return
		}
	}
}

type streamUpdateEvent struct {
	Event  string `json:"event"`
	Stream string `json:"stream"`
	Key    string `json:"key"`
	Value  string `json:"value"`
}

func (h *Handler) trackIdToURL(trackId string) string {
	return h.root + trackId
}

func (h *Handler) publishUpdate(stream, key, value string) error {
	j, err := json.Marshal(streamUpdateEvent{
		Event:  "update",
		Stream: stream,
		Key:    key,
		Value:  value,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal json: %v", err)
	}
	if err := h.redis.Publish(fmt.Sprintf(eventsFormat, stream), j).Err(); err != nil {
		return fmt.Errorf("failed to publish update: %v", err)
	}
	return nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}
