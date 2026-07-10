package eventlog

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	valkey "github.com/valkey-io/valkey-go"
)

// RedisStreamLog is the production EventLog backed by Redis/Valkey Streams. Each
// session is one stream keyed evlog:{app}:{session}; a per-session INCR counter
// assigns dense, monotonic seq numbers used as the stream entry id "<seq>-0".
// Backlog is replayed with XRANGE; the live tail uses XREAD BLOCK. The same port
// can later be served by Kafka (sessionID→partition, seq→offset) with no change
// above this file.
//
// NOTE: covered by the EventLog contract tests via the in-memory log; this
// adapter requires a running Valkey for integration testing.
type RedisStreamLog struct {
	client  valkey.Client
	app     string
	ttl     time.Duration
	blockMS int64
	maxLen  int64 // XADD MAXLEN ~ cap; 0 disables trimming
}

// NewRedisStreamLog wraps a valkey client as an EventLog.
func NewRedisStreamLog(client valkey.Client, app string) *RedisStreamLog {
	return &RedisStreamLog{client: client, app: app, ttl: 24 * time.Hour, blockMS: 15000, maxLen: 10000}
}

func (r *RedisStreamLog) streamKey(sessionID string) string {
	return fmt.Sprintf("evlog:%s:%s", r.app, sessionID)
}
func (r *RedisStreamLog) seqKey(sessionID string) string {
	return fmt.Sprintf("evlog:seq:%s:%s", r.app, sessionID)
}

// appendScript performs INCR + XADD + EXPIRE atomically on the Redis server so a
// crash or an interleaving second writer can never open a gap or emit an
// out-of-order XADD (H4). KEYS[1]=stream key, KEYS[2]=seq counter key;
// ARGV[1]=marshalled event JSON, ARGV[2]=maxlen (0 disables trim), ARGV[3]=ttl
// seconds. It returns the newly-assigned seq. The entry id is "<seq>-0", keeping
// the dense-integer seq identical to the counter and to ?after=<seq>.
const appendScript = `
local seq = redis.call('INCR', KEYS[2])
if tonumber(ARGV[2]) > 0 then
  redis.call('XADD', KEYS[1], 'MAXLEN', '~', ARGV[2], seq .. '-0', 'd', ARGV[1])
else
  redis.call('XADD', KEYS[1], seq .. '-0', 'd', ARGV[1])
end
if tonumber(ARGV[3]) > 0 then
  redis.call('EXPIRE', KEYS[1], ARGV[3])
  redis.call('EXPIRE', KEYS[2], ARGV[3])
end
return seq
`

// appendKeys returns the two Lua KEYS (stream, seq counter) for a session.
func (r *RedisStreamLog) appendKeys(sessionID string) (streamKey, seqKey string) {
	return r.streamKey(sessionID), r.seqKey(sessionID)
}

// appendArgs returns the ordered Lua ARGV (event JSON, maxlen, ttl-seconds) for
// an event. Split out from Append so the command composition is unit-testable
// without a live Valkey (the actual EVAL cannot be exercised here).
func (r *RedisStreamLog) appendArgs(data []byte) (dataArg, maxlenArg, ttlArg string) {
	return string(data), strconv.FormatInt(r.maxLen, 10), strconv.FormatInt(int64(r.ttl.Seconds()), 10)
}

// Append assigns the next seq and stores the event atomically via a Lua EVAL:
// INCR of the seq counter and the XADD (id "<seq>-0") happen in one server-side
// step, so the two can never be split by a crash or a competing writer. The seq
// is the durable, gap-free sequence used for ?after=<seq> resume.
func (r *RedisStreamLog) Append(ctx context.Context, sessionID string, ev AgentEvent) (int64, error) {
	data, err := json.Marshal(ev)
	if err != nil {
		return 0, err
	}
	streamKey, seqKey := r.appendKeys(sessionID)
	dataArg, maxlenArg, ttlArg := r.appendArgs(data)
	// Builder: EVAL <script> NUMKEYS=2 KEYS[stream,seq] ARGV[data,maxlen,ttl].
	seq, err := r.client.Do(ctx,
		r.client.B().Eval().Script(appendScript).Numkeys(2).
			Key(streamKey, seqKey).
			Arg(dataArg, maxlenArg, ttlArg).Build(),
	).AsInt64()
	if err != nil {
		return 0, fmt.Errorf("eventlog append eval: %w", err)
	}
	return seq, nil
}

// Read replays backlog after afterSeq via XRANGE, then (if follow) tails via
// XREAD BLOCK until ctx is done or a terminal run-status is delivered.
func (r *RedisStreamLog) Read(ctx context.Context, sessionID string, afterSeq int64, follow bool) (<-chan SeqEvent, error) {
	if afterSeq < 0 {
		afterSeq = 0
	}
	out := make(chan SeqEvent)
	go func() {
		defer close(out)
		key := r.streamKey(sessionID)

		// Backlog: XRANGE (exclusive start.
		start := "-"
		if afterSeq > 0 {
			start = fmt.Sprintf("(%d-0", afterSeq)
		}
		entries, err := r.client.Do(ctx, r.client.B().Xrange().Key(key).Start(start).End("+").Build()).AsXRange()
		if err == nil {
			for _, e := range entries {
				if !deliver(ctx, out, e) {
					return
				}
			}
		}
		if !follow {
			return
		}

		// Live tail from the last seen id.
		lastID := "$"
		if afterSeq > 0 {
			lastID = fmt.Sprintf("%d-0", afterSeq)
		}
		if len(entries) > 0 {
			lastID = entries[len(entries)-1].ID
		}
		for {
			if ctx.Err() != nil {
				return
			}
			res, err := r.client.Do(ctx,
				r.client.B().Xread().Count(128).Block(r.blockMS).Streams().Key(key).Id(lastID).Build(),
			).AsXRead()
			if err != nil {
				// timeout / nil reply: emit heartbeat and continue.
				select {
				case out <- SeqEvent{Seq: -1, Event: AgentEvent{V: 1, Type: EvHeartbeat}}:
				case <-ctx.Done():
					return
				}
				continue
			}
			for _, e := range res[key] {
				lastID = e.ID
				if !deliver(ctx, out, e) {
					return
				}
				// W1: terminal events do NOT close a follow reader. Runs own
				// terminal state, not sessions — a session stream holds many
				// turns. Closure policy lives in the pump; a follow tail here
				// closes only when ctx is cancelled.
			}
		}
	}()
	return out, nil
}

func deliver(ctx context.Context, out chan<- SeqEvent, e valkey.XRangeEntry) bool {
	ev, ok := decodeEntry(e)
	if !ok {
		return true // skip malformed
	}
	select {
	case out <- SeqEvent{Seq: seqFromID(e.ID), Event: ev}:
		return true
	case <-ctx.Done():
		return false
	}
}

func decodeEntry(e valkey.XRangeEntry) (AgentEvent, bool) {
	raw, ok := e.FieldValues["d"]
	if !ok {
		return AgentEvent{}, false
	}
	var ev AgentEvent
	if json.Unmarshal([]byte(raw), &ev) != nil {
		return AgentEvent{}, false
	}
	return ev, true
}

func seqFromID(id string) int64 {
	ms, _, _ := strings.Cut(id, "-")
	n, _ := strconv.ParseInt(ms, 10, 64)
	return n
}

// Head returns the latest assigned seq (the INCR counter value).
func (r *RedisStreamLog) Head(ctx context.Context, sessionID string) (int64, error) {
	n, err := r.client.Do(ctx, r.client.B().Get().Key(r.seqKey(sessionID)).Build()).AsInt64()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return 0, nil
		}
		return 0, err
	}
	return n, nil
}

// Close is a no-op; the valkey client lifecycle is owned by the caller.
func (r *RedisStreamLog) Close() error { return nil }
