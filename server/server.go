package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/go-logr/logr"
	"github.com/klauspost/compress/zstd"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	earbugv3 "go.seankhliao.com/earbug/v3/pb/earbug/v3"
	"go.seankhliao.com/gchat"
	"go.seankhliao.com/svcrunner"
	"go.seankhliao.com/svcrunner/envflag"
	"google.golang.org/protobuf/proto"
)

type Server struct {
	bucket string

	bkt   *storage.BucketHandle
	gchat gchat.WebhookClient

	log   logr.Logger
	trace trace.Tracer
}

func New(hs *http.Server) *Server {
	s := &Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/summary", s.summary)
	hs.Handler = mux
	return s
}

func (s *Server) Register(c *envflag.Config) {
	c.StringVar(&s.gchat.Endpoint, "earbug.gchat", "", "webhook for google chat space to post summaries")
	c.StringVar(&s.bucket, "earbug.bucket", "", "storage bucket to read user data from")
}

func (s *Server) Init(ctx context.Context, t svcrunner.Tools) error {
	s.log = t.Log.WithName("earbug-gchat")
	s.trace = otel.Tracer("earbug-gchat")

	client, err := storage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("create storage client: %w", err)
	}

	s.bkt = client.Bucket(s.bucket)
	s.gchat.Client = &http.Client{
		Transport: otelhttp.NewTransport(nil),
	}
	return nil
}

type userReq struct {
	User string `json:"user"`
}

func (s *Server) summary(rw http.ResponseWriter, r *http.Request) {
	log := s.log.WithName("summary")
	ctx, span := s.trace.Start(r.Context(), "summary")
	defer span.End()

	user, msg, code, err := func(method string, body io.ReadCloser) (string, string, int, error) {
		ctx, span = s.trace.Start(ctx, "extract-user")
		defer span.End()

		if r.Method != http.MethodPost {
			log = log.WithValues("method", r.Method)
			return "", "invalid method", http.StatusMethodNotAllowed, errors.New("POST only")
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return "", "read body", http.StatusBadRequest, err
		}
		var user userReq
		err = json.Unmarshal(b, &user)
		if err == nil && user.User == "" {
			err = errors.New("no user provided")
		}
		if err != nil {
			return "", "unmarshal body", http.StatusBadRequest, err
		}
		return user.User, "", 0, nil
	}(r.Method, r.Body)
	if err != nil {
		http.Error(rw, msg, code)
		log.Error(err, msg, "ctx", ctx, "http_request", r)
		return
	}

	log = log.WithValues("user", user)

	data, msg, code, err := func(user string) (*earbugv3.Store, string, int, error) {
		ctx, span = s.trace.Start(ctx, "read-data")
		defer span.End()

		key := user + ".pb.zstd"
		obj := s.bkt.Object(key)
		or, err := obj.NewReader(ctx)
		if err != nil {
			return nil, "create object reader", http.StatusInternalServerError, err
		}
		defer or.Close()

		zr, err := zstd.NewReader(or)
		if err != nil {
			return nil, "create zstd reader", http.StatusInternalServerError, err
		}
		defer zr.Close()

		b, err := io.ReadAll(zr)
		if err != nil {
			return nil, "read object", http.StatusInternalServerError, err
		}

		var data earbugv3.Store
		err = proto.Unmarshal(b, &data)
		if err != nil {
			return nil, "unmarshal as proto", http.StatusInternalServerError, err
		}
		return &data, "", 0, nil
	}(user)
	if err != nil {
		http.Error(rw, msg, code)
		log.Error(err, msg, "ctx", ctx, "http_request", r)
		return
	}

	msg, code, err = func(data *earbugv3.Store) (string, int, error) {
		ctx, span = s.trace.Start(ctx, "post-summary")
		defer span.End()

		playedBefore := make(map[string]struct{})
		playedYesterday := make(map[string]struct{})
		var yesterdayPlays int
		tsPrefix := time.Now().Add(time.Duration(-24) * time.Hour).Format("2006-01-02")
		for ts, played := range data.Playbacks {
			cmp := strings.Compare(ts[:10], tsPrefix)
			if cmp < 0 {
				playedBefore[played.TrackId] = struct{}{}
			} else if cmp == 0 {
				yesterdayPlays++
				playedYesterday[played.TrackId] = struct{}{}
			}
		}

		var yesterdayNewTracks int
		for id := range playedYesterday {
			if _, ok := playedBefore[id]; !ok {
				yesterdayNewTracks++
			}
		}

		log = log.WithValues("summary_date", tsPrefix, "plays", yesterdayPlays, "tracks", len(playedYesterday), "tracks_new", yesterdayNewTracks)
		chatMsg := fmt.Sprintf("%s | %v plays | %v tracks (%v new)", tsPrefix, yesterdayPlays, len(playedYesterday), yesterdayNewTracks)
		err = s.gchat.Post(ctx, gchat.WebhookPayload{
			Text: chatMsg,
		})
		if err != nil {
			return "post message", http.StatusInternalServerError, err
		}

		return "ok", http.StatusOK, nil
	}(data)
	if err != nil {
		http.Error(rw, msg, code)
		log.Error(err, msg, "ctx", ctx, "http_request", r)
		return
	}

	rw.Write([]byte(msg))
	log.Info("posted summary", "ctx", ctx, "http_request", r)
}
