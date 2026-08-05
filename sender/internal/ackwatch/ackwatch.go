// Package ackwatch consumes the acknowledgement channel: the only thing the sender ever learns from
// the far side of the air gap.
//
// It watches a directory rather than listening on a socket, because there is no socket. The optical
// link is one-way by construction, so the receiver reports by writing signed files to storage both
// applications can reach, and this reads them. Neither application calls the other, and either can be
// restarted without the other noticing — which is also why every record is verified before it is
// acted on. Anything able to write that directory could otherwise tell the sender a chunk arrived
// when it did not, truncating the transfer, or that everything failed, making it retransmit for ever.
package ackwatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/store"
)

// Watcher applies acknowledgements to the database.
type Watcher struct {
	store *store.Store
	cfg   *config.Watcher
	log   *zap.Logger

	// root is the shared directory. It is a path rather than an object store because the sender has
	// to *watch* it, and watching is a filesystem operation with no equivalent in an object store's
	// interface — polling a bucket listing is the alternative, and it is what the fallback below does.
	root string

	mu sync.Mutex

	// applied remembers which records have been processed, keyed by transmission and sequence, so a
	// re-scan does not re-apply what it already has. The records are small and a transmission is
	// bounded, so remembering them costs little next to re-reading and re-verifying every file on
	// every pass.
	applied map[string]bool

	// results collects the receiver's final verdicts, so a caller waiting on a transmission learns
	// what happened without polling the database.
	resultsMu sync.Mutex
	results   map[uuid.UUID]protocol.Result
	waiters   map[uuid.UUID][]chan protocol.Result
}

// New returns a watcher over the shared acknowledgement directory.
func New(st *store.Store, cfg *config.Watcher, log *zap.Logger) *Watcher {
	return &Watcher{
		store:   st,
		cfg:     cfg,
		log:     log.Named("ackwatch"),
		root:    cfg.Current().Ack.Dir,
		applied: map[string]bool{},
		results: map[uuid.UUID]protocol.Result{},
		waiters: map[uuid.UUID][]chan protocol.Result{},
	}
}

// Run watches for acknowledgements until the context is done.
//
// Both a filesystem notification and a poll are used, and the poll is not a belt-and-braces
// nicety — it is the mechanism that actually works. The acknowledgement directory is a shared volume,
// which in practice means NFS, SMB, or a container bind mount, and inotify does not see writes made
// by another host on any of them. The notification makes the common case fast; the poll makes the
// deployed case correct.
func (w *Watcher) Run(ctx context.Context) error {
	if err := os.MkdirAll(w.acksDir(), 0o750); err != nil {
		return fmt.Errorf("ackwatch: %w", err)
	}

	// A scan first, so acknowledgements written while the sender was down are picked up rather than
	// waiting for a change that has already happened.
	if err := w.Scan(ctx); err != nil {
		w.log.Warn("initial acknowledgement scan failed", zap.Error(err))
	}

	notify, err := fsnotify.NewWatcher()
	if err != nil {
		// A failure here is survivable: the poll alone is correct, only slower.
		w.log.Warn("filesystem notification unavailable, polling only", zap.Error(err))
		return w.poll(ctx, nil)
	}
	defer notify.Close()

	if err := notify.Add(w.acksDir()); err != nil {
		w.log.Warn("could not watch the acknowledgement directory, polling only", zap.Error(err))
		return w.poll(ctx, nil)
	}
	// Per-transmission subdirectories appear as the transfer starts, so they are added as they show
	// up. Watching only the root would see the directory created and nothing inside it.
	w.watchExisting(notify)

	return w.poll(ctx, notify)
}

// poll scans on a timer, and immediately whenever a notification arrives.
func (w *Watcher) poll(ctx context.Context, notify *fsnotify.Watcher) error {
	interval := w.cfg.Current().Ack.PollInterval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var events <-chan fsnotify.Event
	var errs <-chan error
	if notify != nil {
		events, errs = notify.Events, notify.Errors
	}

	for {
		select {
		case <-ctx.Done():
			return nil

		case event := <-events:
			// A new per-transmission directory has to be watched too, or its records would only be seen
			// by the next poll.
			if event.Op&fsnotify.Create != 0 && notify != nil {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					_ = notify.Add(event.Name)
				}
			}
			if err := w.Scan(ctx); err != nil {
				w.log.Warn("acknowledgement scan failed", zap.Error(err))
			}

		case err := <-errs:
			w.log.Warn("filesystem notification error", zap.Error(err))

		case <-ticker.C:
			if err := w.Scan(ctx); err != nil {
				w.log.Warn("acknowledgement scan failed", zap.Error(err))
			}
		}
	}
}

// watchExisting adds the per-transmission directories that already exist.
func (w *Watcher) watchExisting(notify *fsnotify.Watcher) {
	entries, err := os.ReadDir(w.acksDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			_ = notify.Add(filepath.Join(w.acksDir(), e.Name()))
		}
	}
}

// Scan reads and applies every acknowledgement that has not been applied yet.
func (w *Watcher) Scan(ctx context.Context) error {
	entries, err := os.ReadDir(w.acksDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ackwatch: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		transmission, err := uuid.Parse(entry.Name())
		if err != nil {
			// A directory whose name is not a transmission id is not ours. Ignored rather than
			// reported, because the shared volume belongs to both applications and may hold other things.
			continue
		}
		if err := w.scanTransmission(ctx, transmission); err != nil {
			w.log.Warn("could not read acknowledgements",
				zap.String("transmission", transmission.String()), zap.Error(err))
		}
	}
	return nil
}

// scanTransmission applies one transmission's records.
func (w *Watcher) scanTransmission(ctx context.Context, transmission uuid.UUID) error {
	dir := filepath.Join(w.root, protocol.AckDir(transmission))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		// A dotted name is a write in progress. Reading one would produce truncated JSON that failed
		// to verify, and discarding a record that was about to be perfectly good.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	// In name order, which is sequence order: the receiver pads its sequence numbers precisely so
	// that a directory listing gives them back in the order they were written.
	sort.Strings(names)

	secret := []byte(w.cfg.Current().Ack.Secret)
	for _, name := range names {
		key := transmission.String() + "/" + name

		w.mu.Lock()
		done := w.applied[key]
		w.mu.Unlock()
		if done {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			// A file that vanished between the listing and the read is not an error worth reporting:
			// housekeeping on the shared volume may legitimately have removed it.
			continue
		}

		if name == "result.json" {
			if err := w.applyResult(ctx, secret, transmission, data); err != nil {
				w.log.Warn("could not apply the receiver's result",
					zap.String("transmission", transmission.String()), zap.Error(err))
				continue
			}
		} else {
			if err := w.applyAck(ctx, secret, transmission, data); err != nil {
				w.log.Warn("could not apply an acknowledgement",
					zap.String("transmission", transmission.String()),
					zap.String("record", name), zap.Error(err))
				continue
			}
		}

		w.mu.Lock()
		w.applied[key] = true
		w.mu.Unlock()
	}
	return nil
}

// applyAck records one chunk's outcome.
func (w *Watcher) applyAck(ctx context.Context, secret []byte, transmission uuid.UUID, data []byte) error {
	ack, err := protocol.ParseAck(secret, data)
	if err != nil {
		// A record that will not verify is discarded rather than acted on, and the distinction is the
		// whole reason acknowledgements are signed: acting on it would let whoever wrote it decide when
		// the sender stops transmitting.
		return err
	}
	if ack.TransmissionID != transmission {
		return fmt.Errorf("ackwatch: record in %s claims transmission %s",
			transmission, ack.TransmissionID)
	}

	if ack.Status.Delivered() {
		// This is the line that makes an acknowledged chunk stop being displayed. The scheduler reads
		// the same rows, so marking it here removes the chunk from the window on the next frame.
		if err := w.store.Chunks.MarkAcked(ctx, transmission, int(ack.ChunkNumber)); err != nil {
			return err
		}
		if _, err := w.store.Transmissions.RecountAcked(ctx, transmission); err != nil {
			return err
		}
	} else {
		// A chunk the receiver could read but could not trust: counted, so an operator can see a
		// channel that is delivering frames which fail their checksums rather than losing them
		// outright — a different fault with a different cause.
		if err := w.store.Chunks.AddRetry(ctx, transmission, int(ack.ChunkNumber)); err != nil {
			return err
		}
	}

	if err := w.store.Stats.Record(ctx, "ack_latency_seconds",
		time.Since(ack.Timestamp()).Seconds(), &transmission); err != nil {
		w.log.Debug("could not record acknowledgement latency", zap.Error(err))
	}
	if ack.BitErrorRate > 0 {
		if err := w.store.Stats.Record(ctx, "bit_error_rate", ack.BitErrorRate, &transmission); err != nil {
			w.log.Debug("could not record the bit error rate", zap.Error(err))
		}
	}
	return nil
}

// applyResult records the receiver's final verdict and settles the callback.
//
// This is where the loop the caller started actually closes. Somebody handed the sender a file and a
// URL; the receiver merged the file, checked it against the hash the manifest declared, delivered it,
// and wrote down what happened. The sender records that verdict against the transmission, so the
// answer to "did my transfer work" is a question the sender can answer rather than one that needs
// asking across the air gap.
func (w *Watcher) applyResult(ctx context.Context, secret []byte, transmission uuid.UUID, data []byte) error {
	result, err := protocol.ParseResult(secret, data)
	if err != nil {
		return err
	}
	if result.TransmissionID != transmission {
		return fmt.Errorf("ackwatch: result in %s claims transmission %s",
			transmission, result.TransmissionID)
	}

	status := store.TxCompleted
	reason := ""
	if !result.Verified {
		// Every chunk may have arrived and the file still be wrong. The receiver's hash check is the
		// only thing that can tell, so its verdict decides the transmission's status rather than the
		// acknowledgement count.
		status = store.TxFailed
		reason = "the receiver could not verify the merged file: " + result.Error
	}
	if err := w.store.Transmissions.SetStatus(ctx, transmission, status, reason); err != nil {
		return err
	}

	if result.CallbackURL != "" {
		if err := w.store.Callbacks.Settle(ctx, transmission, result.CallbackDelivered,
			result.CallbackStatus, result.CallbackError, result); err != nil {
			w.log.Warn("could not record the callback outcome", zap.Error(err))
		}
	}

	if err := w.store.Stats.Record(ctx, "throughput_bytes_per_second",
		result.ThroughputBytesPerSecond(), &transmission); err != nil {
		w.log.Debug("could not record throughput", zap.Error(err))
	}

	w.log.Info("receiver reported the transfer",
		zap.String("transmission", transmission.String()),
		zap.String("file", result.Filename),
		zap.Uint64("bytes", result.Size),
		zap.Bool("verified", result.Verified),
		zap.Uint32("chunks", result.ChunksReceived),
		zap.Uint32("recovered", result.ChunksRecovered),
		zap.Bool("callback_delivered", result.CallbackDelivered),
		zap.Int("callback_status", result.CallbackStatus),
		zap.Duration("took", result.Duration()),
		zap.Float64("bytes_per_second", result.ThroughputBytesPerSecond()))

	w.publish(result)
	return nil
}

// publish hands a result to anything waiting for it.
func (w *Watcher) publish(result protocol.Result) {
	w.resultsMu.Lock()
	defer w.resultsMu.Unlock()

	w.results[result.TransmissionID] = result
	for _, waiter := range w.waiters[result.TransmissionID] {
		waiter <- result
		close(waiter)
	}
	delete(w.waiters, result.TransmissionID)
}

// Result returns the receiver's verdict if it has arrived.
func (w *Watcher) Result(transmission uuid.UUID) (protocol.Result, bool) {
	w.resultsMu.Lock()
	defer w.resultsMu.Unlock()
	result, ok := w.results[transmission]
	return result, ok
}

// WaitForResult blocks until the receiver reports on a transmission.
//
// It is how a caller finds out a transfer finished without polling: the sender knows when the result
// record lands, so anything waiting can be told rather than made to ask.
func (w *Watcher) WaitForResult(ctx context.Context, transmission uuid.UUID) (protocol.Result, error) {
	w.resultsMu.Lock()
	if result, ok := w.results[transmission]; ok {
		w.resultsMu.Unlock()
		return result, nil
	}
	waiter := make(chan protocol.Result, 1)
	w.waiters[transmission] = append(w.waiters[transmission], waiter)
	w.resultsMu.Unlock()

	select {
	case <-ctx.Done():
		return protocol.Result{}, ctx.Err()
	case result := <-waiter:
		return result, nil
	}
}

func (w *Watcher) acksDir() string { return filepath.Join(w.root, "acks") }
