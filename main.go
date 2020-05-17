package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/go-redis/redis/v7"

	"github.com/PonyFest/music-control/auth"
	"github.com/PonyFest/music-control/events"
	"github.com/PonyFest/music-control/songs"
	"github.com/PonyFest/music-control/streams"
)

// services provided:
// upload file to storage:
// - generate identifier (UUID, probably)
// - upload to spaces
// - store metadata in db
// retrieve song listing:
// - just give the whole list - it's probably not very big
// update up next for a stream
// - just stick it in a redis list?
// retrieve a new track for a stream
// - check up next, otherwise select a random track (not in the last n tracks), then store it as a recent track
// start/stop playing for a stream
// - store state in redis
// - push state to client for that stream
// skip for a stream
// - just publish? player can request a new track as if it had ended
//   - this will misbehave if there are multiple players for one stream
//   - so does the track end behaviour, so :shrug:?
//   - doing better in both cases requires either exactly one server or serious juggling to do things exactly once

type config struct {
	RedisURL  string
	S3Bucket  string
	MusicRoot string
	Bind      string
	Password  string
}

func parseConfig() (config, error) {
	c := config{}
	flag.StringVar(&c.RedisURL, "redis-url", "", "The URL of the redis server")
	flag.StringVar(&c.S3Bucket, "s3-bucket", "", "The S3 bucket to store music in")
	flag.StringVar(&c.MusicRoot, "music-root", "", "The root URL to access music at")
	flag.StringVar(&c.Bind, "bind", "0.0.0.0:8080", "The address:port to bind the server to")
	flag.StringVar(&c.Password, "password", "", "The password to require for HTTP Basic Auth")
	flag.Parse()

	if c.RedisURL == "" {
		return c, fmt.Errorf("--redis-url is required")
	}
	if c.S3Bucket == "" {
		return c, fmt.Errorf("--s3-bucket is required")
	}
	if c.MusicRoot == "" {
		return c, fmt.Errorf("--music-root is required")
	}
	if !strings.HasSuffix(c.MusicRoot, "/") {
		c.MusicRoot += "/"
	}
	return c, nil
}

func acceptAllCors(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE")
		}
		handler.ServeHTTP(w, r)
	})
}

func main() {
	rand.Seed(time.Now().UnixNano())
	c, err := parseConfig()
	if err != nil {
		log.Fatalf("error: %v.\n", err)
	}
	s3Client, err := getS3Client()
	if err != nil {
		log.Fatalln(err)
	}
	redisClient, err := getRedisClient(c.RedisURL)
	if err != nil {
		log.Fatalln(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/tracks", songs.New(s3Client, c.S3Bucket, redisClient))
	mux.Handle("/api/streams/", http.StripPrefix("/api/streams", streams.New(redisClient, c.MusicRoot)))
	mux.Handle("/api/events", events.New(redisClient))

	var handler http.Handler
	if c.Password != "" {
		handler = auth.Basic(mux, c.Password, "PonyFest Music Control")
	} else {
		handler = mux
	}
	http.Handle("/", acceptAllCors(handler))
	log.Fatalln(http.ListenAndServe(c.Bind, nil))
}

func getS3Client() (*s3.S3, error) {
	var s3Configs []*aws.Config
	// The AWS SDK picks up most of its config from the environment, but this endpoint can only be specified in code,
	// so we emulate the environment behaviour.
	if os.Getenv("AWS_ENDPOINT") != "" {
		s3Configs = append(s3Configs, &aws.Config{
			Endpoint: aws.String(os.Getenv("AWS_ENDPOINT")),
		})
	}
	s3Session, err := session.NewSession(s3Configs...)
	if err != nil {
		return nil, fmt.Errorf("creating an S3 session failed: %v", err)
	}
	s3Client := s3.New(s3Session)
	return s3Client, nil
}

func getRedisClient(url string) (*redis.Client, error) {
	redisOptions, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("invalid redis URL %q: %v", url, err)
	}
	return redis.NewClient(redisOptions), nil
}
