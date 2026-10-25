package web

import (
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/williamokano/qbt-replicator/internal/config"
)

// keyLimiter holds one token bucket per API key for reads and one for
// changes. Buckets live in memory and reset on restart.
type keyLimiter struct {
	mu   sync.Mutex
	keys map[int64]*keyBuckets
	last time.Time // last cleanup
}

type keyBuckets struct {
	read, write *rate.Limiter
	cfg         config.RateLimitConfig
	seen        time.Time
}

func newKeyLimiter() *keyLimiter { return &keyLimiter{keys: map[int64]*keyBuckets{}} }

func bucket(rps float64, burst int) *rate.Limiter {
	if rps <= 0 {
		return rate.NewLimiter(rate.Inf, 0)
	}
	return rate.NewLimiter(rate.Limit(rps), burst)
}

// allow takes a token for key id; when the bucket is empty it returns how
// long to wait. Changed limits apply on the next request.
func (l *keyLimiter) allow(id int64, write bool, cfg config.RateLimitConfig, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.last) > time.Hour {
		for k, b := range l.keys {
			if now.Sub(b.seen) > time.Hour {
				delete(l.keys, k)
			}
		}
		l.last = now
	}
	b, ok := l.keys[id]
	if !ok || b.cfg != cfg {
		b = &keyBuckets{read: bucket(cfg.Read, cfg.ReadBurst), write: bucket(cfg.Write, cfg.WriteBurst), cfg: cfg}
		l.keys[id] = b
	}
	b.seen = now
	lim := b.read
	if write {
		lim = b.write
	}
	r := lim.ReserveN(now, 1)
	if !r.OK() {
		return false, time.Second
	}
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)
		return false, d
	}
	return true, 0
}

// failLimiter counts invalid API keys per client IP: 10 attempts, then one
// more every 6 seconds, so keys cannot be guessed.
type failLimiter struct {
	mu  sync.Mutex
	ips map[string]*rate.Limiter
}

const (
	failBurst    = 10
	failInterval = 6 * time.Second
)

func newFailLimiter() *failLimiter { return &failLimiter{ips: map[string]*rate.Limiter{}} }

func (f *failLimiter) get(ip string) *rate.Limiter {
	l, ok := f.ips[ip]
	if !ok {
		if len(f.ips) > 10000 { // bound memory; full buckets carry no state worth keeping
			for k, v := range f.ips {
				if v.Tokens() >= failBurst {
					delete(f.ips, k)
				}
			}
		}
		l = rate.NewLimiter(rate.Every(failInterval), failBurst)
		f.ips[ip] = l
	}
	return l
}

// blocked reports whether ip used up its attempts and how long until the next one.
func (f *failLimiter) blocked(ip string, now time.Time) (bool, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.get(ip).TokensAt(now)
	if t >= 1 {
		return false, 0
	}
	return true, time.Duration(math.Ceil((1-t)*failInterval.Seconds())) * time.Second
}

// fail records an invalid key from ip.
func (f *failLimiter) fail(ip string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.get(ip).AllowN(now, 1)
}
