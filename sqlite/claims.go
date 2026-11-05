package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nogo/faden"
)

// Default claim timing: a claim runs out 30 s after its last renewal, and a
// store renews every 10 s.
const (
	DefaultClaimExpiry = 30 * time.Second
	DefaultClaimRenew  = 10 * time.Second
)

// claims is what the store and its WithTx bindings share about the sessions
// this store owns. The claim itself is a row in the claims table: session,
// owner, token and expiry. A takeover raises the token, so a write of the
// old owner finds a token it does not hold.
type claims struct {
	owner string // random per Open; no two stores share it

	mu     sync.Mutex // guards the fields below
	held   map[string]int64
	expiry time.Duration
	renew  time.Duration
	stop   chan struct{} // closed by Close; nil until the first claim
	done   chan struct{} // closed when the renewing goroutine ends
	closed bool
}

func newClaims() (*claims, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	return &claims{owner: hex.EncodeToString(id), held: map[string]int64{}, expiry: DefaultClaimExpiry, renew: DefaultClaimRenew}, nil
}

func (c *claims) token(session string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	token, ok := c.held[session]
	return token, ok
}

// SetClaimTiming sets how long a claim lives without renewal and how often
// this store renews its claims. Call it before the first Claim. Zero keeps
// the default; renew should be well below expiry, or a live owner loses its
// sessions.
func (s *Store) SetClaimTiming(expiry, renew time.Duration) {
	s.claims.mu.Lock()
	defer s.claims.mu.Unlock()
	if expiry > 0 {
		s.claims.expiry = expiry
	}
	if renew > 0 {
		s.claims.renew = renew
	}
}

// Claim implements faden.Store. A live claim of another store fails with
// faden.ErrLocked; one past its expiry is taken over.
func (s *Store) Claim(ctx context.Context, session string) error {
	// A cheap read first: a session owned elsewhere is the common answer of a
	// poll, and it should not take the write lock.
	if owner, _, expires, err := s.claimRow(ctx, session); err == nil && owner != "" && owner != s.claims.owner && time.Now().UnixNano() < expires {
		return fmt.Errorf("sqlite: claim session %s: %w", session, faden.ErrLocked)
	}
	var token int64
	err := s.write(ctx, func(s *Store) error {
		if _, err := s.Session(ctx, session); err != nil {
			return err
		}
		owner, old, expires, err := s.claimRow(ctx, session)
		now := time.Now()
		switch {
		case errors.Is(err, sql.ErrNoRows):
			token = 1
		case err != nil:
			return err
		case owner == s.claims.owner:
			token = old
		case owner != "" && now.UnixNano() < expires:
			return faden.ErrLocked
		default:
			token = old + 1
		}
		s.claims.mu.Lock()
		until := now.Add(s.claims.expiry).UnixNano()
		s.claims.mu.Unlock()
		_, err = s.tx.ExecContext(ctx, `INSERT INTO `+s.prefix+`claims (session,owner,token,expires) VALUES (?1,?2,?3,?4)
			ON CONFLICT(session) DO UPDATE SET owner=?2,token=?3,expires=?4`, session, s.claims.owner, token, until)
		return err
	})
	if err != nil {
		return fmt.Errorf("sqlite: claim session %s: %w", session, err)
	}
	s.claims.mu.Lock()
	defer s.claims.mu.Unlock()
	if s.claims.closed {
		return fmt.Errorf("sqlite: claim session %s: store closed", session)
	}
	s.claims.held[session] = token
	if s.claims.stop == nil {
		s.claims.stop, s.claims.done = make(chan struct{}), make(chan struct{})
		go s.renewLoop(s.claims.stop, s.claims.done, s.claims.renew)
	}
	return nil
}

// Release implements faden.Store.
func (s *Store) Release(ctx context.Context, session string) error {
	token, ok := s.claims.token(session)
	if !ok {
		return nil
	}
	err := s.write(ctx, func(s *Store) error {
		_, err := s.tx.ExecContext(ctx, `UPDATE `+s.prefix+`claims SET owner='',expires=0 WHERE session=? AND owner=? AND token=?`, session, s.claims.owner, token)
		return err
	})
	if err != nil {
		return fmt.Errorf("sqlite: release session %s: %w", session, err)
	}
	s.claims.mu.Lock()
	delete(s.claims.held, session)
	s.claims.mu.Unlock()
	return nil
}

func (s *Store) claimRow(ctx context.Context, session string) (owner string, token, expires int64, err error) {
	err = s.row(ctx, `SELECT owner,token,expires FROM `+s.prefix+`claims WHERE session=?`, session).Scan(&owner, &token, &expires)
	return
}

// fence fails with faden.ErrLocked unless this store may change session: it
// holds the claim with the token it was given, or nobody holds a live one.
// It runs inside the write's transaction, so the check and the write cannot
// be separated by a takeover.
func (s *Store) fence(ctx context.Context, session string) error {
	owner, token, expires, err := s.claimRow(ctx, session)
	held, mine := s.claims.token(session)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		err = nil
		if mine {
			err = faden.ErrLocked // the row of a claim this store holds is gone
		}
	case err != nil:
	case mine:
		if owner != s.claims.owner || token != held {
			err = faden.ErrLocked
		}
	case owner != "" && time.Now().UnixNano() < expires:
		err = faden.ErrLocked
	}
	if err != nil && errors.Is(err, faden.ErrLocked) {
		return fmt.Errorf("session %s: %w", session, err)
	}
	return err
}

// renewLoop keeps this store's claims alive until stop is closed.
func (s *Store) renewLoop(stop, done chan struct{}, every time.Duration) {
	defer close(done)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			// A failed renewal is retried at the next tick; the owner loses
			// the claim only when the expiry passes first, and then its
			// writes are fenced.
			s.claims.mu.Lock()
			until := time.Now().Add(s.claims.expiry).UnixNano()
			s.claims.mu.Unlock()
			s.write(context.Background(), func(s *Store) error {
				_, err := s.tx.ExecContext(context.Background(), `UPDATE `+s.prefix+`claims SET expires=? WHERE owner=?`, until, s.claims.owner)
				return err
			})
		}
	}
}
