// Package server is the webhook: an authenticated REST API that lets CI run
// deploy and rollback on this machine and wait for the outcome.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/beyenpay/shipit/internal/config"
	"github.com/beyenpay/shipit/internal/deploy"
)

// Runner performs the actual work. *deploy.Deployer implements it.
type Runner interface {
	Deploy(ctx context.Context, project, tag string) (string, error)
	Rollback(ctx context.Context, project, tag string) (string, error)
}

type Server struct {
	// ConfigPath is re-read on every request, so projects and secrets can be
	// changed without restarting. An unreadable or invalid file is an error,
	// never a reason to fall back to older settings.
	ConfigPath string
	NewRunner  func(cfg *config.Config, logf func(string, ...any)) Runner

	jobs   *jobStore
	limit  *limiter
	replay *replayCache
	now    func() time.Time
}

func New(configPath string) *Server {
	return &Server{
		ConfigPath: configPath,
		NewRunner: func(cfg *config.Config, logf func(string, ...any)) Runner {
			return deploy.New(cfg, logf)
		},
		jobs:   newJobStore(),
		limit:  newLimiter(),
		replay: newReplayCache(),
		now:    time.Now,
	}
}

const (
	ctxConfig = "shipit.config"
	ctxBody   = "shipit.body"
)

func (s *Server) Handler() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	// Trust no proxy headers: the client address is the TCP peer. Clients can
	// therefore not dodge the per-IP limits by sending X-Forwarded-For.
	_ = r.SetTrustedProxies(nil)
	r.HandleMethodNotAllowed = true
	r.NoRoute(func(c *gin.Context) { c.JSON(http.StatusNotFound, gin.H{"error": "not found"}) })
	r.NoMethod(func(c *gin.Context) { c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "method not allowed"}) })

	r.GET("/healthz", s.rateLimit, func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })

	v1 := r.Group("/v1", s.rateLimit, s.authenticate)
	v1.POST("/deploy", func(c *gin.Context) { s.enqueue(c, "deploy") })
	v1.POST("/rollback", func(c *gin.Context) { s.enqueue(c, "rollback") })
	v1.GET("/jobs/:id", s.getJob)
	return r
}

// Run serves on addr until ctx is cancelled, then stops accepting requests and
// gives running jobs time to finish, so a restart of shipit never leaves a
// release half switched.
func (s *Server) Run(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      deploy.DefaultTimeout + 30*time.Second, // ?wait=true holds the response open
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	log.Printf("listening on %s", ln.Addr())

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	log.Printf("shutting down, waiting for running jobs")
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_ = srv.Shutdown(sctx)
	s.jobs.wait(sctx)
	return nil
}

// rateLimit applies the per-IP request limit.
func (s *Server) rateLimit(c *gin.Context) {
	if !s.limit.allow(c.ClientIP()) {
		c.Header("Retry-After", "5")
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
	}
}

// authenticate loads the configuration and verifies the request signature.
// All failures look the same to the client; the reason goes to the log.
func (s *Server) authenticate(c *gin.Context) {
	cfg, err := config.Load(s.ConfigPath)
	if err == nil {
		err = cfg.ValidateServe()
	}
	if err != nil {
		log.Printf("config error: %v", err)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "server configuration error, see the shipit log"})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
		} else {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "cannot read request body"})
		}
		return
	}

	now := s.now()
	sig := c.GetHeader(HeaderSignature)
	err = verify(cfg.Secret, c.GetHeader(HeaderTimestamp), sig, c.Request.Method, c.Request.URL.Path, body, now)
	if err == nil && c.Request.Method == http.MethodPost && !s.replay.add(sig, now) {
		err = errors.New("replayed request")
	}
	if err != nil {
		ip := c.ClientIP()
		s.limit.fail(ip)
		log.Printf("auth failed from %s: %v", ip, err)
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	c.Set(ctxConfig, cfg)
	c.Set(ctxBody, body)
}

type request struct {
	Project string `json:"project"`
	Tag     string `json:"tag"`
}

func (s *Server) enqueue(c *gin.Context, action string) {
	cfg := c.MustGet(ctxConfig).(*config.Config)
	body := c.MustGet(ctxBody).([]byte)

	var req request
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body must be JSON like {\"project\":\"...\",\"tag\":\"...\"}"})
		return
	}
	if _, ok := cfg.Projects[req.Project]; !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown project"})
		return
	}
	// An explicit tag is required for deploys: the webhook never guesses
	// "latest", which could pick a different release than the one CI built.
	if action == "deploy" && req.Tag == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tag is required"})
		return
	}
	if req.Tag != "" && !config.ValidTag(req.Tag) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tag"})
		return
	}

	job := s.jobs.start(req.Project, action, req.Tag, func(ctx context.Context, logf func(string, ...any)) (string, error) {
		r := s.NewRunner(cfg, logf)
		if action == "deploy" {
			return r.Deploy(ctx, req.Project, req.Tag)
		}
		return r.Rollback(ctx, req.Project, req.Tag)
	})

	if !wantsWait(c) {
		c.Header("Location", "/v1/jobs/"+job.ID)
		c.JSON(http.StatusAccepted, s.jobs.view(job))
		return
	}
	select {
	case <-job.done:
	case <-c.Request.Context().Done():
		return // the caller left; the job carries on
	}
	code := http.StatusOK
	if err := s.jobs.jobErr(job); err != nil {
		code = http.StatusInternalServerError
		if errors.Is(err, deploy.ErrBusy) {
			code = http.StatusConflict
		}
	}
	c.JSON(code, s.jobs.view(job))
}

func (s *Server) getJob(c *gin.Context) {
	j := s.jobs.get(c.Param("id"))
	if j == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found (jobs are kept in memory for an hour and are lost when shipit restarts; check `shipit status` on the server)"})
		return
	}
	c.JSON(http.StatusOK, s.jobs.view(j))
}

func wantsWait(c *gin.Context) bool {
	v, err := strconv.ParseBool(c.Query("wait"))
	return err == nil && v
}
