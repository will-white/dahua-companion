// Package snapshot fetches a camera still after an event and hands it off for
// publishing, one at a time so memory stays bounded to roughly one image no
// matter how busy the doorway gets.
package snapshot

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// fetchTimeout bounds one snapshot download; a still that arrives later than
// this no longer shows who was at the door.
const fetchTimeout = 5 * time.Second

// FetchFunc downloads a JPEG for a camera channel.
type FetchFunc func(ctx context.Context, channel int) ([]byte, error)

// PublishFunc delivers a fetched image to topic.
type PublishFunc func(topic string, img []byte)

// Snapshotter runs at most one fetch at a time. A request that arrives while
// one is in flight is skipped, except that a press takes over from a
// detection: the ring is the snapshot that matters.
type Snapshotter struct {
	ctx     context.Context
	fetch   FetchFunc
	publish PublishFunc
	wg      sync.WaitGroup

	mu       sync.Mutex
	inFlight bool
	priority bool
	cancel   context.CancelFunc
	gen      uint64 // identifies the current fetch, so a cancelled one cannot clear its successor's state
}

// New returns a Snapshotter whose fetches stop when ctx is cancelled.
func New(ctx context.Context, fetch FetchFunc, publish PublishFunc) *Snapshotter {
	return &Snapshotter{ctx: ctx, fetch: fetch, publish: publish}
}

// Request fetches a snapshot of channel in the background and publishes it to
// topic. priority marks a press, which may replace an in-flight detection
// fetch; anything else is skipped while a fetch is running. It never blocks.
func (s *Snapshotter) Request(topic string, channel int, priority bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	if s.inFlight {
		if !priority || s.priority {
			log.Debug().Str("topic", topic).Msg("Snapshot already in progress; skipped")
			return
		}
		log.Debug().Str("topic", topic).Msg("Press snapshot replaces in-progress detection snapshot")
		s.cancel()
	}
	ctx, cancel := context.WithTimeout(s.ctx, fetchTimeout)
	s.gen++
	s.inFlight, s.priority, s.cancel = true, priority, cancel
	gen := s.gen
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.finish(gen, cancel)
		img, err := s.fetch(ctx, channel)
		if err != nil {
			if ctx.Err() == nil || ctx.Err() == context.DeadlineExceeded {
				log.Error().Err(err).Str("topic", topic).Msg("Snapshot fetch failed")
			}
			return
		}
		if ctx.Err() != nil {
			return // superseded by a press, or shutting down
		}
		s.publish(topic, img)
	}()
}

func (s *Snapshotter) finish(gen uint64, cancel context.CancelFunc) {
	cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen == gen {
		s.inFlight = false
	}
}

// Wait blocks until every started fetch has finished; call it after the
// context passed to New is cancelled.
func (s *Snapshotter) Wait() {
	s.wg.Wait()
}
