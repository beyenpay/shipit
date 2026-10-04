package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/beyenpay/shipit/internal/deploy"
)

const (
	jobRetention = time.Hour
	maxJobs      = 200
	maxJobLog    = 500 // lines
	maxLogLine   = 1000
)

const (
	statusRunning = "running"
	statusSuccess = "success"
	statusFailed  = "failed"
)

// Job is one deploy or rollback request. Jobs live in memory only: they are a
// convenience for the caller, never the source of truth (the symlinks are).
type Job struct {
	ID       string
	Project  string
	Action   string
	Tag      string
	Status   string
	Error    string
	Log      []string
	Started  time.Time
	Finished time.Time

	err  error
	done chan struct{}
}

// JobView is the JSON form of a Job.
type JobView struct {
	ID         string     `json:"job_id"`
	Project    string     `json:"project"`
	Action     string     `json:"action"`
	Tag        string     `json:"tag,omitempty"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
	Log        []string   `json:"log"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type jobStore struct {
	mu   sync.Mutex
	jobs map[string]*Job
	wg   sync.WaitGroup
	now  func() time.Time
}

func newJobStore() *jobStore {
	return &jobStore{jobs: make(map[string]*Job), now: time.Now}
}

// jobFunc does the work of a job. It returns the tag that ended up live.
type jobFunc func(ctx context.Context, logf func(string, ...any)) (string, error)

// start registers a job and runs fn in the background. The job is bound to a
// fresh context, not to the HTTP request: a caller that disconnects must not
// abort a half-finished switch.
func (s *jobStore) start(project, action, tag string, fn jobFunc) *Job {
	j := &Job{
		ID:      newID(),
		Project: project,
		Action:  action,
		Tag:     tag,
		Status:  statusRunning,
		Started: s.now(),
		Log:     []string{},
		done:    make(chan struct{}),
	}

	s.mu.Lock()
	s.prune()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), deploy.DefaultTimeout)
		defer cancel()

		logf := func(format string, args ...any) {
			line := fmt.Sprintf(format, args...)
			if len(line) > maxLogLine {
				line = line[:maxLogLine] + "..."
			}
			log.Printf("job %s: %s", j.ID, line)
			s.mu.Lock()
			if len(j.Log) < maxJobLog {
				j.Log = append(j.Log, s.now().UTC().Format("15:04:05")+" "+line)
			}
			s.mu.Unlock()
		}
		label := action + " " + project
		if tag != "" {
			label += " " + tag
		}
		logf("%s started", label)

		got, err := fn(ctx, logf)

		s.mu.Lock()
		if got != "" {
			j.Tag = got
		}
		j.Finished = s.now()
		if err != nil {
			j.Status, j.Error, j.err = statusFailed, err.Error(), err
		} else {
			j.Status = statusSuccess
		}
		s.mu.Unlock()
		if err != nil {
			logf("FAILED: %v", err)
		} else {
			logf("done")
		}
		close(j.done)
	}()
	return j
}

func (s *jobStore) view(j *Job) JobView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := JobView{
		ID: j.ID, Project: j.Project, Action: j.Action, Tag: j.Tag,
		Status: j.Status, Error: j.Error,
		Log:       append([]string{}, j.Log...),
		StartedAt: j.Started,
	}
	if !j.Finished.IsZero() {
		f := j.Finished
		v.FinishedAt = &f
	}
	return v
}

func (s *jobStore) get(id string) *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id]
}

// jobErr returns the error of a finished job.
func (s *jobStore) jobErr(j *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return j.err
}

// wait blocks until all running jobs finished or ctx ends.
func (s *jobStore) wait(ctx context.Context) {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// prune drops expired finished jobs, then the oldest finished ones if there
// are still too many. Running jobs are never dropped. Caller holds s.mu.
func (s *jobStore) prune() {
	now := s.now()
	var finished []*Job
	for id, j := range s.jobs {
		if j.Finished.IsZero() {
			continue
		}
		if now.Sub(j.Finished) > jobRetention {
			delete(s.jobs, id)
			continue
		}
		finished = append(finished, j)
	}
	if len(s.jobs) < maxJobs {
		return
	}
	sort.Slice(finished, func(i, k int) bool { return finished[i].Finished.Before(finished[k].Finished) })
	for _, j := range finished {
		if len(s.jobs) < maxJobs {
			break
		}
		delete(s.jobs, j.ID)
	}
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system RNG failing is not recoverable
	}
	return hex.EncodeToString(b)
}
