package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

const (
	// HeaderTimestamp carries the request time in unix seconds.
	HeaderTimestamp = "X-Shipit-Timestamp"
	// HeaderSignature carries the hex HMAC-SHA256 of the request.
	HeaderSignature = "X-Shipit-Signature"

	maxClockSkew = 5 * time.Minute
	maxBodyBytes = 64 << 10
	maxReplayIDs = 10000
)

// Sign returns the signature for a request:
//
//	hex(HMAC-SHA256(secret, timestamp "\n" METHOD "\n" path "\n" body))
//
// path is the URL path without the query string. Binding the method and path
// means a captured signature cannot be replayed against another endpoint.
func Sign(secret, timestamp, method, path string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(timestamp + "\n" + method + "\n" + path + "\n"))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// verify checks the timestamp window and the signature. The caller must not
// reveal which of the two failed.
func verify(secret, timestamp, signature, method, path string, body []byte, now time.Time) error {
	if timestamp == "" || signature == "" {
		return errors.New("missing auth headers")
	}
	secs, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("malformed timestamp")
	}
	skew := now.Sub(time.Unix(secs, 0))
	if skew < -maxClockSkew || skew > maxClockSkew {
		return fmt.Errorf("timestamp outside the allowed window (skew %s)", skew.Round(time.Second))
	}
	got, err := hex.DecodeString(signature)
	if err != nil {
		return errors.New("malformed signature")
	}
	want, _ := hex.DecodeString(Sign(secret, timestamp, method, path, body))
	if !hmac.Equal(got, want) {
		return errors.New("bad signature")
	}
	return nil
}

// replayCache remembers recently accepted signatures so that a captured
// request cannot be submitted a second time inside the timestamp window.
type replayCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newReplayCache() *replayCache {
	return &replayCache{seen: make(map[string]time.Time)}
}

// add records sig and reports whether it was new.
func (c *replayCache) add(sig string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) >= maxReplayIDs/2 {
		for k, exp := range c.seen {
			if !now.Before(exp) {
				delete(c.seen, k)
			}
		}
	}
	if exp, dup := c.seen[sig]; dup && now.Before(exp) {
		return false
	}
	if len(c.seen) >= maxReplayIDs {
		return false
	}
	c.seen[sig] = now.Add(2*maxClockSkew + time.Second)
	return true
}
