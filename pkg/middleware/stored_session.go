package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/justinas/alice"
	middlewareapi "github.com/oauth2-proxy/oauth2-proxy/v7/pkg/apis/middleware"
	sessionsapi "github.com/oauth2-proxy/oauth2-proxy/v7/pkg/apis/sessions"
	"github.com/oauth2-proxy/oauth2-proxy/v7/pkg/logger"
	"github.com/oauth2-proxy/oauth2-proxy/v7/providers"
)

const (
	SessionLockExpireTime = 5 * time.Second
	SessionLockPeekDelay  = 50 * time.Millisecond
)

// StoredSessionLoaderOptions contains all of the requirements to construct
// a stored session loader.
// All options must be provided.
type StoredSessionLoaderOptions struct {
	// Session storage backend
	SessionStore sessionsapi.SessionStore

	// How often should sessions be refreshed
	RefreshPeriod time.Duration

	// Provider based session refreshing
	RefreshSession func(context.Context, *sessionsapi.SessionState) (bool, error)

	// Provider based session validation.
	// If the sesssion is older than `RefreshPeriod` but the provider doesn't
	// refresh it, we must re-validate using this validation.
	ValidateSession func(context.Context, *sessionsapi.SessionState) bool
}

// NewStoredSessionLoader creates a new storedSessionLoader which loads
// sessions from the session store.
// If no session is found, the request will be passed to the nex handler.
// If a session was loader by a previous handler, it will not be replaced.
func NewStoredSessionLoader(opts *StoredSessionLoaderOptions) alice.Constructor {
	ss := &storedSessionLoader{
		store:            opts.SessionStore,
		refreshPeriod:    opts.RefreshPeriod,
		sessionRefresher: opts.RefreshSession,
		sessionValidator: opts.ValidateSession,
	}
	return ss.loadSession
}

// storedSessionLoader is responsible for loading sessions from cookie
// identified sessions in the session store.
type storedSessionLoader struct {
	store            sessionsapi.SessionStore
	refreshPeriod    time.Duration
	sessionRefresher func(context.Context, *sessionsapi.SessionState) (bool, error)
	sessionValidator func(context.Context, *sessionsapi.SessionState) bool
}

// loadSession attempts to load a session as identified by the request cookies.
// If no session is found, the request will be passed to the next handler.
// If a session was loader by a previous handler, it will not be replaced.
func (s *storedSessionLoader) loadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		scope := middlewareapi.GetRequestScope(req)
		// If scope is nil, this will panic.
		// A scope should always be injected before this handler is called.
		if scope.Session != nil {
			// The session was already loaded, pass to the next handler
			next.ServeHTTP(rw, req)
			return
		}

		session, err := s.getValidatedSession(rw, req)
		if err != nil && !errors.Is(err, http.ErrNoCookie) {
			// In the case when there was an error loading the session,
			// we should clear the session
			logger.Errorf("Error loading cookied session: %v, removing session", err)
			err = s.store.Clear(rw, req)
			if err != nil {
				logger.Errorf("Error removing session: %v", err)
			}
		}

		// Add the session to the scope if it was found
		scope.Session = session
		next.ServeHTTP(rw, req)
	})
}

// getValidatedSession is responsible for loading a session and making sure
// that is is valid.
func (s *storedSessionLoader) getValidatedSession(rw http.ResponseWriter, req *http.Request) (*sessionsapi.SessionState, error) {
	session, err := s.store.Load(req)
	if err != nil || session == nil {
		// No session was found in the storage or error occurred, nothing more to do
		return nil, err
	}

	err = s.refreshSessionIfNeeded(rw, req, session)
	if err != nil {
		return nil, fmt.Errorf("error refreshing access token for session (%s): %v", session, err)
	}

	// Validate all sessions after any Redeem/Refresh operation (fail or success)
	err = s.validateSession(req.Context(), session)
	if err != nil {
		return nil, err
	}

	return session, nil
}

// refreshSessionIfNeeded will attempt to refresh a session if the session
// is older than the refresh period.
// Success or fail, we will then validate the session.
func (s *storedSessionLoader) refreshSessionIfNeeded(rw http.ResponseWriter, req *http.Request, session *sessionsapi.SessionState) error {
	if s.refreshPeriod <= time.Duration(0) || session.Age() < s.refreshPeriod {
		// Refresh is disabled or the session is not old enough, do nothing
		return nil
	}

	wasLocked, err := s.waitForPossibleSessionLock(session, req)
	if err != nil {
		return err
	}

	// If session was locked, fetch current state, because
	// it should be updated after lock is released.
	if wasLocked {
		logger.Printf("update session from store")
		return s.updateSessionFromStore(req, session)
	}

	logger.Printf("Refreshing session - User: %s; SessionAge: %s", session.User, session.Age())
	err = s.refreshSession(rw, req, session)
	if err != nil {
		// If a preemptive refresh fails, we still keep the session
		// if validateSession succeeds.
		logger.Errorf("Unable to refresh session: %v", err)
	}
	return err
}

// refreshSession attempts to refresh the session with the provider
// and will save the session if it was updated.
func (s *storedSessionLoader) refreshSession(rw http.ResponseWriter, req *http.Request, session *sessionsapi.SessionState) error {
	err := session.ObtainLock(req.Context(), SessionLockExpireTime)
	if err != nil {
		logger.Errorf("unable to obtain lock (skipping refresh): %v", err)
		return nil
	}
	defer func() {
		err = session.ReleaseLock(req.Context())
		if err != nil {
			logger.Errorf("unable to release lock: %v", err)
		}
	}()

	refreshed, err := s.sessionRefresher(req.Context(), session)
	if err != nil && !errors.Is(err, providers.ErrNotImplemented) {
		return fmt.Errorf("error refreshing tokens: %v", err)
	}

	// HACK:
	// Providers that don't implement `RefreshSession` use the default
	// implementation which returns `ErrNotImplemented`.
	// Pretend it refreshed to reset the refresh timer so that `ValidateSession`
	// isn't triggered every subsequent request and is only called once during
	// this request.
	if errors.Is(err, providers.ErrNotImplemented) {
		refreshed = true
	}

	// Session not refreshed, nothing to persist.
	if !refreshed {
		return nil
	}

	// If we refreshed, update the `CreatedAt` time to reset the refresh timer
	// (In case underlying provider implementations forget)
	session.CreatedAtNow()

	// Because the session was refreshed, make sure to save it
	err = s.store.Save(rw, req, session)
	if err != nil {
		logger.PrintAuthf(session.Email, req, logger.AuthError, "error saving session: %v", err)
		err = fmt.Errorf("error saving session: %v", err)
	}
	return err
}

func (s *storedSessionLoader) updateSessionFromStore(req *http.Request, session *sessionsapi.SessionState) error {
	sessionStored, err := s.store.Load(req)
	if err != nil {
		return fmt.Errorf("unable to load updated session from store: %v", err)
	}

	if session == nil || sessionStored == nil {
		return fmt.Errorf("no session available to udpate from store")
	}
	*session = *sessionStored

	return nil
}

func (s *storedSessionLoader) waitForPossibleSessionLock(session *sessionsapi.SessionState, req *http.Request) (bool, error) {
	var wasLocked bool
	var err error
	var isLocked bool
	for isLocked, err = session.PeekLock(req.Context()); isLocked; isLocked, err = session.PeekLock(req.Context()) {
		wasLocked = true
		// delay next peek lock
		time.Sleep(SessionLockPeekDelay)
	}

	if err != nil {
		return false, err
	}

	return wasLocked, nil
}

// validateSession checks whether the session has expired and performs
// provider validation on the session.
// An error implies the session is not longer valid.
func (s *storedSessionLoader) validateSession(ctx context.Context, session *sessionsapi.SessionState) error {
	if session.IsExpired() {
		return errors.New("session is expired")
	}

	if !s.sessionValidator(ctx, session) {
		return errors.New("session is invalid")
	}

	return nil
}
