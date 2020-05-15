package songs

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/dhowden/tag"
	"github.com/go-redis/redis/v7"
	"github.com/google/uuid"
)

const TrackPoolKey = "track-pool"
const EventsKey = "events"

type MusicHandler struct {
	s3     *s3.S3
	bucket string
	redis  *redis.Client
}

func New(s3 *s3.S3, bucket string, redis *redis.Client) *MusicHandler {
	return &MusicHandler{
		s3:     s3,
		bucket: bucket,
		redis:  redis,
	}
}

func (m *MusicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		m.addTrack(w, r)
	case http.MethodGet:
		m.listTracks(w, r)
	}
}

func (m *MusicHandler) listTracks(w http.ResponseWriter, r *http.Request) {
	trackIds, err := m.redis.SMembers(TrackPoolKey).Result()
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to list track IDs: %v", err), http.StatusInternalServerError)
		return
	}
	p := m.redis.Pipeline()
	results := map[string]*redis.StringStringMapCmd{}
	for _, trackId := range trackIds {
		results[trackId] = p.HGetAll(trackId)
	}
	if _, err := p.Exec(); err != nil {
		http.Error(w, fmt.Sprintf("Looking up track data failed: %v", err), http.StatusInternalServerError)
		return
	}

	// static typing is for wimps
	ret := map[string]map[string]string{}
	for trackId, trackResult := range results {
		track, err := trackResult.Result()
		if err != nil {
			log.Printf("Couldn't look up data for track %q: %v\n", trackId, err)
		}
		ret[trackId] = track
	}
	if err := json.NewEncoder(w).Encode(map[string]interface{}{"tracks": ret}); err != nil {
		http.Error(w, fmt.Sprintf("Failed to encode json", err), http.StatusInternalServerError)
		return
	}
}

func (m *MusicHandler) addTrack(w http.ResponseWriter, r *http.Request) {
	f, err := ioutil.TempFile("", "tmpmusic")
	if err != nil {
		http.Error(w, "creating temp file failed", http.StatusInternalServerError)
		return
	}
	defer os.Remove(f.Name())
	if _, err := io.Copy(f, r.Body); err != nil {
		http.Error(w, "saving audio failed", http.StatusInternalServerError)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "seeking a file failed I guess?", http.StatusInternalServerError)
		return
	}
	trackID, err := m.processMusicFile(f)
	if err != nil {
		http.Error(w, fmt.Sprintf("Processing music failed: %v", err), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte(fmt.Sprintf(`{"status": "ok", "uuid": "%s"}`, trackID)))
}

var mimeTypeMapping = map[tag.Format]string{
	tag.ID3v1:   "audio/mpeg",
	tag.ID3v2_2: "audio/mpeg",
	tag.ID3v2_3: "audio/mpeg",
	tag.ID3v2_4: "audio/mpeg",
	tag.MP4:     "audio/aac",
	tag.VORBIS:  "audio/ogg",
}

func (m *MusicHandler) processMusicFile(file io.ReadSeeker) (uuid.UUID, error) {
	t, err := tag.ReadFrom(file)
	if err != nil {
		return uuid.Nil, fmt.Errorf("couldn't parse file: %v", err)
	}
	ft := t.Format()
	if ft == tag.VORBIS {
		return uuid.Nil, fmt.Errorf("not a media type: %q", ft)
	}
	log.Printf("Adding %s - %s (%s)...\n", t.Title(), t.Artist(), t.FileType())

	trackID := uuid.New()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return uuid.Nil, fmt.Errorf("seeking to the start of the file somehow failed: %v", err)
	}
	if _, err = m.s3.PutObject(&s3.PutObjectInput{
		Bucket:      &m.bucket,
		Body:        file,
		Key:         aws.String(trackID.String()),
		ACL:         aws.String("public-read"),
		ContentType: aws.String(mimeTypeMapping[ft]),
	}); err != nil {
		return uuid.Nil, fmt.Errorf("upload to 'S3' failed: %v", err)
	}
	if err := m.redis.Watch(func(tx *redis.Tx) error {
		if err := tx.HSet(trackID.String(), "title", t.Title(), "artist", t.Artist()).Err(); err != nil {
			return err
		}
		if err := tx.SAdd(TrackPoolKey, trackID.String()).Err(); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return uuid.Nil, fmt.Errorf("file uploaded but metadata storage failed: %v", err)
	}
	j, err := json.Marshal(map[string]string{
		"event":   "poolTrackAdded",
		"trackId": trackID.String(),
		"title":   t.Title(),
		"artist":  t.Artist(),
	})
	if err == nil {
		if err := m.redis.Publish(EventsKey, j).Err(); err != nil {
			log.Printf("Failed to publish track added event: %v.\n", err)
		}
	} else {
		log.Printf("Failed to encode JSON, somehow: %v.\n", err)
	}
	log.Printf("Uploaded %s\n", trackID)
	return trackID, nil
}
