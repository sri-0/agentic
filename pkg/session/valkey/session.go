package valkey

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"time"

	vk "github.com/valkey-io/valkey-go"
	"google.golang.org/adk/session"
)

// SessionService implements session.Service using Valkey/Redis as the backend.
type SessionService struct {
	client vk.Client
	ttl    time.Duration
}

// NewSessionService creates a new Valkey-backed session service.
// Accepts an existing valkey.Client. TTL defaults to 24 hours if zero.
func NewSessionService(client vk.Client, ttl time.Duration) *SessionService {
	if ttl == 0 {
		ttl = 24 * time.Hour
	}
	return &SessionService{
		client: client,
		ttl:    ttl,
	}
}

// Key helpers
func (s *SessionService) sessionKey(appName, userID, sessionID string) string {
	return fmt.Sprintf("session:%s:%s:%s", appName, userID, sessionID)
}

func (s *SessionService) sessionsIndexKey(appName, userID string) string {
	return fmt.Sprintf("sessions:%s:%s", appName, userID)
}

func (s *SessionService) eventsKey(appName, userID, sessionID string) string {
	return fmt.Sprintf("events:%s:%s:%s", appName, userID, sessionID)
}

// Create creates a new session.
func (s *SessionService) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = fmt.Sprintf("%d", time.Now().UnixNano())
	}

	key := s.sessionKey(req.AppName, req.UserID, sessionID)

	sess := &valkeySession{
		id:             sessionID,
		appName:        req.AppName,
		userID:         req.UserID,
		state:          newValkeyState(req.State, s.client, key, s.ttl),
		events:         newValkeyEvents(nil, s.client, s.eventsKey(req.AppName, req.UserID, sessionID)),
		lastUpdateTime: time.Now(),
	}

	data, err := json.Marshal(sess.toStorable())
	if err != nil {
		return nil, fmt.Errorf("failed to marshal session: %w", err)
	}

	if err := s.client.Do(ctx, s.client.B().Set().Key(key).Value(string(data)).Ex(s.ttl).Build()).Error(); err != nil {
		return nil, fmt.Errorf("failed to store session: %w", err)
	}

	// Add to sessions index
	indexKey := s.sessionsIndexKey(req.AppName, req.UserID)
	if err := s.client.Do(ctx, s.client.B().Sadd().Key(indexKey).Member(sessionID).Build()).Error(); err != nil {
		return nil, fmt.Errorf("failed to update sessions index: %w", err)
	}
	s.client.Do(ctx, s.client.B().Expire().Key(indexKey).Seconds(int64(s.ttl.Seconds())).Build())

	return &session.CreateResponse{Session: sess}, nil
}

// Get retrieves a session by ID.
func (s *SessionService) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	key := s.sessionKey(req.AppName, req.UserID, req.SessionID)

	data, err := s.client.Do(ctx, s.client.B().Get().Key(key).Build()).AsBytes()
	if err != nil {
		if vk.IsValkeyNil(err) {
			return nil, fmt.Errorf("session not found: %s", req.SessionID)
		}
		return nil, fmt.Errorf("failed to get session: %w", err)
	}

	var storable storableSession
	if err := json.Unmarshal(data, &storable); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session: %w", err)
	}

	// Load events
	eventsKey := s.eventsKey(req.AppName, req.UserID, req.SessionID)
	eventData, err := s.client.Do(ctx, s.client.B().Lrange().Key(eventsKey).Start(0).Stop(-1).Build()).AsStrSlice()
	if err != nil && !vk.IsValkeyNil(err) {
		return nil, fmt.Errorf("failed to get events: %w", err)
	}

	var events []*session.Event
	for _, ed := range eventData {
		var evt session.Event
		if err := json.Unmarshal([]byte(ed), &evt); err != nil {
			continue
		}
		events = append(events, &evt)
	}

	// Apply filters
	if req.NumRecentEvents > 0 && len(events) > req.NumRecentEvents {
		events = events[len(events)-req.NumRecentEvents:]
	}
	if !req.After.IsZero() {
		var filtered []*session.Event
		for _, evt := range events {
			if !evt.Timestamp.Before(req.After) {
				filtered = append(filtered, evt)
			}
		}
		events = filtered
	}

	sess := &valkeySession{
		id:             storable.ID,
		appName:        storable.AppName,
		userID:         storable.UserID,
		state:          newValkeyState(storable.State, s.client, key, s.ttl),
		events:         newValkeyEvents(events, s.client, eventsKey),
		lastUpdateTime: storable.LastUpdateTime,
	}

	return &session.GetResponse{Session: sess}, nil
}

// List returns all sessions for a user.
func (s *SessionService) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	indexKey := s.sessionsIndexKey(req.AppName, req.UserID)

	sessionIDs, err := s.client.Do(ctx, s.client.B().Smembers().Key(indexKey).Build()).AsStrSlice()
	if err != nil {
		if vk.IsValkeyNil(err) {
			return &session.ListResponse{}, nil
		}
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}

	var sessions []session.Session
	for _, sessionID := range sessionIDs {
		resp, err := s.Get(ctx, &session.GetRequest{
			AppName:   req.AppName,
			UserID:    req.UserID,
			SessionID: sessionID,
		})
		if err != nil {
			continue
		}
		sessions = append(sessions, resp.Session)
	}

	return &session.ListResponse{Sessions: sessions}, nil
}

// Delete removes a session.
func (s *SessionService) Delete(ctx context.Context, req *session.DeleteRequest) error {
	key := s.sessionKey(req.AppName, req.UserID, req.SessionID)
	eventsKey := s.eventsKey(req.AppName, req.UserID, req.SessionID)
	indexKey := s.sessionsIndexKey(req.AppName, req.UserID)

	cmds := make(vk.Commands, 0, 3)
	cmds = append(cmds, s.client.B().Del().Key(key).Build())
	cmds = append(cmds, s.client.B().Del().Key(eventsKey).Build())
	cmds = append(cmds, s.client.B().Srem().Key(indexKey).Member(req.SessionID).Build())

	for _, resp := range s.client.DoMulti(ctx, cmds...) {
		if err := resp.Error(); err != nil {
			return fmt.Errorf("failed to delete session: %w", err)
		}
	}

	return nil
}

// AppendEvent appends an event to a session.
func (s *SessionService) AppendEvent(ctx context.Context, sess session.Session, evt *session.Event) error {
	evt.Timestamp = time.Now()
	if evt.ID == "" {
		evt.ID = fmt.Sprintf("%d", time.Now().UnixNano())
	}

	data, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	eventsKey := s.eventsKey(sess.AppName(), sess.UserID(), sess.ID())
	if err := s.client.Do(ctx, s.client.B().Rpush().Key(eventsKey).Element(string(data)).Build()).Error(); err != nil {
		return fmt.Errorf("failed to append event: %w", err)
	}
	s.client.Do(ctx, s.client.B().Expire().Key(eventsKey).Seconds(int64(s.ttl.Seconds())).Build())

	// Update session's last update time and persist current state
	key := s.sessionKey(sess.AppName(), sess.UserID(), sess.ID())
	sessData, err := s.client.Do(ctx, s.client.B().Get().Key(key).Build()).AsBytes()
	if err != nil {
		return fmt.Errorf("failed to get session for update: %w", err)
	}

	var storable storableSession
	if err := json.Unmarshal(sessData, &storable); err != nil {
		return fmt.Errorf("failed to unmarshal session: %w", err)
	}

	// Sync state from session to storable
	state := sess.State()
	if state != nil {
		storable.State = make(map[string]any)
		for k, v := range state.All() {
			storable.State[k] = v
		}
	}

	storable.LastUpdateTime = time.Now()
	updatedData, err := json.Marshal(storable)
	if err != nil {
		return fmt.Errorf("failed to marshal updated session: %w", err)
	}

	if err := s.client.Do(ctx, s.client.B().Set().Key(key).Value(string(updatedData)).Ex(s.ttl).Build()).Error(); err != nil {
		return fmt.Errorf("failed to update session: %w", err)
	}

	return nil
}

// Close closes the Valkey connection.
func (s *SessionService) Close() {
	s.client.Close()
}

// storableSession is the JSON-serializable representation of a session.
type storableSession struct {
	ID             string         `json:"id"`
	AppName        string         `json:"app_name"`
	UserID         string         `json:"user_id"`
	State          map[string]any `json:"state"`
	LastUpdateTime time.Time      `json:"last_update_time"`
}

// valkeySession implements session.Session.
type valkeySession struct {
	id             string
	appName        string
	userID         string
	state          *valkeyState
	events         *valkeyEvents
	lastUpdateTime time.Time
}

func (s *valkeySession) ID() string                { return s.id }
func (s *valkeySession) AppName() string           { return s.appName }
func (s *valkeySession) UserID() string            { return s.userID }
func (s *valkeySession) State() session.State      { return s.state }
func (s *valkeySession) Events() session.Events    { return s.events }
func (s *valkeySession) LastUpdateTime() time.Time { return s.lastUpdateTime }

func (s *valkeySession) toStorable() storableSession {
	state := make(map[string]any)
	for k, v := range s.state.All() {
		state[k] = v
	}
	return storableSession{
		ID:             s.id,
		AppName:        s.appName,
		UserID:         s.userID,
		State:          state,
		LastUpdateTime: s.lastUpdateTime,
	}
}

// valkeyState implements session.State with Valkey persistence.
type valkeyState struct {
	data   map[string]any
	client vk.Client
	key    string
	ttl    time.Duration
}

func newValkeyState(initial map[string]any, client vk.Client, key string, ttl time.Duration) *valkeyState {
	data := make(map[string]any)
	for k, v := range initial {
		data[k] = v
	}
	return &valkeyState{
		data:   data,
		client: client,
		key:    key,
		ttl:    ttl,
	}
}

func (s *valkeyState) Get(key string) (any, error) {
	v, ok := s.data[key]
	if !ok {
		return nil, session.ErrStateKeyNotExist
	}
	return v, nil
}

func (s *valkeyState) Set(key string, value any) error {
	s.data[key] = value
	return s.persist()
}

func (s *valkeyState) persist() error {
	ctx := context.Background()

	data, err := s.client.Do(ctx, s.client.B().Get().Key(s.key).Build()).AsBytes()
	if err != nil {
		if vk.IsValkeyNil(err) {
			return nil
		}
		return fmt.Errorf("failed to get session for state update: %w", err)
	}

	var storable storableSession
	if err := json.Unmarshal(data, &storable); err != nil {
		return fmt.Errorf("failed to unmarshal session: %w", err)
	}

	storable.State = make(map[string]any)
	for k, v := range s.data {
		storable.State[k] = v
	}
	storable.LastUpdateTime = time.Now()

	updatedData, err := json.Marshal(storable)
	if err != nil {
		return fmt.Errorf("failed to marshal updated session: %w", err)
	}

	if err := s.client.Do(ctx, s.client.B().Set().Key(s.key).Value(string(updatedData)).Ex(s.ttl).Build()).Error(); err != nil {
		return fmt.Errorf("failed to persist state: %w", err)
	}

	return nil
}

func (s *valkeyState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		for k, v := range s.data {
			if !yield(k, v) {
				return
			}
		}
	}
}

// valkeyEvents implements session.Events with live Valkey reads.
type valkeyEvents struct {
	client vk.Client
	key    string
	cached []*session.Event
}

func newValkeyEvents(events []*session.Event, client vk.Client, key string) *valkeyEvents {
	if events == nil {
		events = make([]*session.Event, 0)
	}
	return &valkeyEvents{
		client: client,
		key:    key,
		cached: events,
	}
}

func (e *valkeyEvents) loadFromValkey() []*session.Event {
	if e.key == "" {
		return e.cached
	}

	ctx := context.Background()
	eventData, err := e.client.Do(ctx, e.client.B().Lrange().Key(e.key).Start(0).Stop(-1).Build()).AsStrSlice()
	if err != nil {
		return e.cached
	}

	var events []*session.Event
	for _, ed := range eventData {
		var evt session.Event
		if err := json.Unmarshal([]byte(ed), &evt); err != nil {
			continue
		}
		events = append(events, &evt)
	}
	return events
}

func (e *valkeyEvents) All() iter.Seq[*session.Event] {
	events := e.loadFromValkey()
	return func(yield func(*session.Event) bool) {
		for _, evt := range events {
			if !yield(evt) {
				return
			}
		}
	}
}

func (e *valkeyEvents) Len() int {
	events := e.loadFromValkey()
	return len(events)
}

func (e *valkeyEvents) At(i int) *session.Event {
	events := e.loadFromValkey()
	if i < 0 || i >= len(events) {
		return nil
	}
	return events[i]
}

// Ensure interfaces are implemented
var _ session.Service = (*SessionService)(nil)
var _ session.Session = (*valkeySession)(nil)
var _ session.State = (*valkeyState)(nil)
var _ session.Events = (*valkeyEvents)(nil)
