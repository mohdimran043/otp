// Package e2e runs the sender and the receiver together over one shared volume.
//
// It is a module of its own for a reason that matters architecturally: the sender and the receiver do
// not import each other, and nothing inside either of them may. They share a protocol, a directory,
// and nothing else — which is what lets either be restarted, upgraded, or replaced without the other
// noticing. A test living inside one and reaching into the other would quietly make that false.
//
// So each application exports a harness over its own internals, and this module imports the two
// harnesses. That is exactly the position an operator is in: two programs, one volume between them,
// and no visibility into either one's insides.
package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/protocol"
	"github.com/opticaltransport/otp/shared/simulate"

	receiver "github.com/opticaltransport/otp/receiver/harness"
	sender "github.com/opticaltransport/otp/sender/harness"
)

// Options configure a loopback.
type Options struct {
	// SenderDatabaseURL and ReceiverDatabaseURL are separate databases, because the two applications
	// have separate ones in every real deployment. Sharing one here would hide any place the code had
	// accidentally assumed otherwise.
	SenderDatabaseURL   string
	ReceiverDatabaseURL string

	// Root is the working directory: the sender's storage, the receiver's storage, and the volume they
	// share all live under it.
	Root string

	// Degrade stands in for the optics, applied to every frame before the receiver decodes it.
	Degrade simulate.Profile

	// Drop stands in for frames the camera missed. It is how loss is injected on purpose, because a
	// channel that never loses anything cannot demonstrate that loss is recovered.
	Drop func(sequence int64) bool

	// TuneSender and TuneReceiver adjust each side's configuration.
	TuneSender   func(*sender.Options)
	TuneReceiver func(*receiver.Options)

	Log *zap.Logger
}

// Loopback is a running sender and receiver with a channel between them.
type Loopback struct {
	Sender   *sender.Sender
	Receiver *receiver.Receiver

	// Callback is the endpoint the receiver delivers merged files to, standing in for whatever system
	// asked for the transfer.
	Callback *CallbackSink

	// SharedDir is the volume both sides reach: frames go one way, acknowledgements the other.
	SharedDir string
	FrameDir  string
}

// Start brings up both applications and the channel between them.
func Start(ctx context.Context, opts Options) (*Loopback, error) {
	if opts.Log == nil {
		opts.Log = zap.NewNop()
	}

	shared := filepath.Join(opts.Root, "shared")
	frames := filepath.Join(shared, "frames")
	for _, dir := range []string{shared, frames} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, err
		}
	}

	// One secret, held by both. It is the only thing besides the protocol and the volume that the two
	// applications have in common, and without it the sender would act on whatever the directory
	// happened to contain.
	const secret = "loopback acknowledgement secret"

	callback := NewCallbackSink()

	senderOpts := sender.Options{
		DatabaseURL: opts.SenderDatabaseURL,
		StorageRoot: filepath.Join(opts.Root, "sender"),
		FrameDir:    frames,
		AckDir:      shared,
		AckSecret:   secret,
		Log:         opts.Log.Named("sender"),
	}
	if opts.TuneSender != nil {
		opts.TuneSender(&senderOpts)
	}

	receiverOpts := receiver.Options{
		DatabaseURL:          opts.ReceiverDatabaseURL,
		StorageRoot:          filepath.Join(opts.Root, "receiver"),
		FrameDir:             frames,
		AckDir:               shared,
		AckSecret:            secret,
		AllowedCallbackHosts: []string{"127.0.0.1", "localhost"},
		Degrade:              opts.Degrade,
		Drop:                 opts.Drop,
		Log:                  opts.Log.Named("receiver"),
	}
	if opts.TuneReceiver != nil {
		opts.TuneReceiver(&receiverOpts)
	}

	// The receiver starts first, so it is already watching when the first frame appears. Starting the
	// sender first would work — the frames wait in the directory — but it would mean every test began
	// by exercising the join-late path rather than the ordinary one.
	receiverSide, err := receiver.Start(ctx, receiverOpts)
	if err != nil {
		callback.Close()
		return nil, err
	}
	senderSide, err := sender.Start(ctx, senderOpts)
	if err != nil {
		receiverSide.Stop()
		callback.Close()
		return nil, err
	}

	return &Loopback{
		Sender:    senderSide,
		Receiver:  receiverSide,
		Callback:  callback,
		SharedDir: shared,
		FrameDir:  frames,
	}, nil
}

// Stop shuts everything down.
func (l *Loopback) Stop() {
	// The sender first, so nothing is still writing frames into a directory the receiver is about to
	// stop watching.
	l.Sender.Stop()
	l.Receiver.Stop()
	l.Callback.Close()
}

// Transfer posts a file and a callback URL to the sender's API, exactly as a caller would.
func (l *Loopback) Transfer(ctx context.Context, filename string, body []byte, callbackURL string) (TransferAccepted, error) {
	return postTransfer(ctx, l.Sender.URL()+"/api/v1/transfers", filename, body, callbackURL)
}

// WaitForResult blocks until the receiver has reported on a transfer.
func (l *Loopback) WaitForResult(ctx context.Context, id uuid.UUID, timeout time.Duration) (protocol.Result, error) {
	return l.Sender.WaitForResult(ctx, id, timeout)
}

// CallbackSink is an HTTP endpoint that accepts delivered files.
//
// It stands in for the system that asked for the transfer, and it checks what it receives rather than
// counting it: the body's own hash has to match the header the receiver sent. That is the end-to-end
// property the whole platform exists to provide, and the only place it can be observed from outside.
type CallbackSink struct {
	Server *httptest.Server

	mu         sync.Mutex
	deliveries []Delivered
	failFirst  int
}

// Delivered is one file the callback endpoint accepted.
type Delivered struct {
	TransmissionID string
	Filename       string
	DeclaredSHA256 string
	ActualSHA256   string
	Body           []byte
	At             time.Time
}

// NewCallbackSink starts an endpoint that accepts deliveries.
func NewCallbackSink() *CallbackSink {
	sink := &CallbackSink{}
	sink.Server = httptest.NewServer(http.HandlerFunc(sink.handle))
	return sink
}

// FailFirst makes the endpoint reject its first n deliveries, so a test can show that a rejected
// delivery is reported rather than quietly treated as success.
func (c *CallbackSink) FailFirst(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failFirst = n
}

func (c *CallbackSink) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256(body)

	c.mu.Lock()
	if c.failFirst > 0 {
		c.failFirst--
		c.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	c.deliveries = append(c.deliveries, Delivered{
		TransmissionID: r.Header.Get("X-OTP-Transmission-Id"),
		Filename:       r.Header.Get("X-OTP-Filename"),
		DeclaredSHA256: r.Header.Get("X-OTP-SHA256"),
		ActualSHA256:   hex.EncodeToString(sum[:]),
		Body:           body,
		At:             time.Now(),
	})
	c.mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

// Deliveries returns what the endpoint has accepted.
func (c *CallbackSink) Deliveries() []Delivered {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Delivered, len(c.deliveries))
	copy(out, c.deliveries)
	return out
}

// URL is where deliveries should be posted.
func (c *CallbackSink) URL() string { return c.Server.URL + "/deliveries" }

// Close stops the endpoint.
func (c *CallbackSink) Close() { c.Server.Close() }
