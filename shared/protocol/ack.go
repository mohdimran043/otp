package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/google/uuid"
)

// The acknowledgement channel.
//
// Acknowledgements do not travel optically, and that is the single most important
// structural decision in the platform. The optical link is one-way by construction —
// a display and a camera — so a receiver has no way to answer over it, and building a
// second optical link back would double the hardware and halve the display's
// available time. Instead the receiver writes small signed records to storage both
// applications can reach, and the sender watches for them.
//
// The consequence worth understanding is that the two applications share no code
// path here at all. The receiver writes files; the sender reads files; neither calls
// the other, and either can be restarted, upgraded, or replaced without the other
// noticing. What they share is this record format, which is why it lives in the
// protocol module rather than in either application.
//
// The records are signed because the acknowledgement channel is the one input a
// sender takes from outside itself. Anything that can write to that directory can
// otherwise tell the sender a chunk arrived when it did not — truncating the
// transmission — or that every chunk failed, making it retransmit for ever.
type AckStatus string

// Acknowledgement outcomes.
const (
	// AckOK means the frame decoded and its chunk passed every integrity check.
	AckOK AckStatus = "ok"

	// AckCRCFailed means the frame was located and read but its payload did not match
	// its checksum: the chunk needs sending again.
	AckCRCFailed AckStatus = "crc_failed"

	// AckDecodeFailed means the frame could not be read at all — the grid was not
	// found, or the header was unreadable.
	AckDecodeFailed AckStatus = "decode_failed"

	// AckDuplicate means the chunk had already arrived. The sender uses it to stop
	// retransmitting something the receiver already holds, which happens routinely:
	// an acknowledgement and a retransmission cross in flight.
	AckDuplicate AckStatus = "duplicate"

	// AckRecovered means the chunk was never received but was reconstructed from
	// parity. It counts as delivered and needs no retransmission, and it is reported
	// distinctly because the rate of it is what tells an operator their error
	// correction is earning its channel time.
	AckRecovered AckStatus = "recovered"
)

// Valid reports whether a status is one this protocol version defines.
func (s AckStatus) Valid() bool {
	switch s {
	case AckOK, AckCRCFailed, AckDecodeFailed, AckDuplicate, AckRecovered:
		return true
	}
	return false
}

// Delivered reports whether the chunk needs no further transmission.
func (s AckStatus) Delivered() bool { return s == AckOK || s == AckDuplicate || s == AckRecovered }

// Ack is one acknowledgement record.
type Ack struct {
	// Sequence numbers the records within a transmission, so the sender can tell an
	// old record from a new one and detect a gap.
	Sequence uint64 `json:"sequence"`

	// TransmissionID and SessionID identify what is being acknowledged and which
	// capture session saw it.
	TransmissionID uuid.UUID `json:"transmission_id"`
	SessionID      uuid.UUID `json:"session_id"`

	// FrameNumber is which displayed frame this concerns, and ChunkNumber which chunk
	// that frame carried. They differ whenever a frame was retransmitted.
	FrameNumber uint32 `json:"frame_number"`
	ChunkNumber uint32 `json:"chunk_number"`

	// Status is the outcome.
	Status AckStatus `json:"status"`

	// RetryCount is how many times the receiver has now seen this chunk fail. The
	// scheduler escalates on it: a chunk failing repeatedly is a sign of something an
	// operator needs to fix rather than something more retries will cure.
	RetryCount uint32 `json:"retry_count"`

	// TimestampMS is when the receiver decided this, in Unix milliseconds. The sender
	// measures acknowledgement latency from it, which is the figure that says how far
	// behind the receiver is running.
	TimestampMS uint64 `json:"timestamp_ms"`

	// BitErrorRate is the fraction of band bits the decoder had to repair, in 0..1.
	// It is a quality signal rather than a verdict: a rising rate across successful
	// frames is the earliest warning that the camera is drifting out of focus.
	BitErrorRate float64 `json:"bit_error_rate"`
}

// Timestamp returns the record time as a Go time value.
func (a Ack) Timestamp() time.Time { return time.UnixMilli(int64(a.TimestampMS)).UTC() }

// Acknowledgement errors.
var (
	// ErrAckSignature means a record's signature did not match. The sender discards
	// such a record rather than acting on it.
	ErrAckSignature = errors.New("protocol: acknowledgement signature is invalid")

	// ErrAckMalformed means a record could not be parsed or contradicts itself.
	ErrAckMalformed = errors.New("protocol: acknowledgement is malformed")
)

// Validate checks a record describes something that could have happened.
func (a Ack) Validate() error {
	if !a.Status.Valid() {
		return fmt.Errorf("%w: unknown status %q", ErrAckMalformed, a.Status)
	}
	if a.TransmissionID == uuid.Nil {
		return fmt.Errorf("%w: no transmission id", ErrAckMalformed)
	}
	if a.TimestampMS == 0 {
		return fmt.Errorf("%w: no timestamp", ErrAckMalformed)
	}
	if a.BitErrorRate < 0 || a.BitErrorRate > 1 {
		return fmt.Errorf("%w: bit error rate %v is not a fraction", ErrAckMalformed, a.BitErrorRate)
	}
	return nil
}

// SignedAck is the record as it is written to storage: the record's exact bytes,
// alongside a signature over those bytes.
//
// The record is carried as raw JSON rather than as a nested object so that
// verification signs and checks the identical byte sequence. Re-serialising a parsed
// record to check its signature would make the signature depend on how this
// program's JSON encoder happens to order and space things — a detail that has
// changed between Go releases before, and would turn a library upgrade on one side of
// the air gap into every acknowledgement failing to verify.
type SignedAck struct {
	Record    json.RawMessage `json:"record"`
	Signature string          `json:"signature"`
}

// SignAck serialises and signs a record.
func SignAck(secret []byte, a Ack) ([]byte, error) {
	if len(secret) == 0 {
		return nil, fmt.Errorf("%w: no signing secret", ErrAckSignature)
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}

	record, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(record)

	return json.Marshal(SignedAck{
		Record:    record,
		Signature: hex.EncodeToString(mac.Sum(nil)),
	})
}

// ParseAck verifies a signed record and returns it.
//
// The signature is checked before the record is parsed for meaning, and the
// comparison is constant-time. A sender that parsed first would be acting on
// attacker-chosen structure before establishing that it came from the receiver.
func ParseAck(secret, data []byte) (Ack, error) {
	if len(secret) == 0 {
		return Ack{}, fmt.Errorf("%w: no signing secret", ErrAckSignature)
	}

	var signed SignedAck
	if err := json.Unmarshal(data, &signed); err != nil {
		return Ack{}, fmt.Errorf("%w: %s", ErrAckMalformed, err)
	}
	if len(signed.Record) == 0 {
		return Ack{}, fmt.Errorf("%w: no record", ErrAckMalformed)
	}

	want, err := hex.DecodeString(signed.Signature)
	if err != nil {
		return Ack{}, fmt.Errorf("%w: signature is not hex", ErrAckSignature)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(signed.Record)
	if !hmac.Equal(mac.Sum(nil), want) {
		return Ack{}, ErrAckSignature
	}

	var a Ack
	if err := json.Unmarshal(signed.Record, &a); err != nil {
		return Ack{}, fmt.Errorf("%w: %s", ErrAckMalformed, err)
	}
	if err := a.Validate(); err != nil {
		return Ack{}, err
	}
	return a, nil
}

// AckDir is the directory a transmission's acknowledgements live in, relative to the
// root both applications share.
func AckDir(transmissionID uuid.UUID) string {
	return path.Join("acks", transmissionID.String())
}

// AckPath is where one acknowledgement is written.
//
// The name is derived from the sequence number alone, and the identifiers are parsed
// from the record rather than from the path, so a sender never has to trust a
// filename. Both parts are formatted here rather than by each caller, because the
// sender finds records by listing a directory and would otherwise have to reproduce
// the receiver's naming exactly.
func AckPath(transmissionID uuid.UUID, sequence uint64) string {
	return path.Join(AckDir(transmissionID), fmt.Sprintf("%020d.json", sequence))
}

// AckTempPath is the name a record is written under before being renamed into place.
//
// The write has to be atomic, and rename is the only operation that is. Without it a
// sender polling the directory eventually reads a file the receiver is still writing,
// finds truncated JSON, and — since a truncated record cannot be verified — discards
// an acknowledgement that was perfectly good. The temporary name is kept out of the
// sequence-numbered namespace so a partial write is never mistaken for a record.
func AckTempPath(transmissionID uuid.UUID, sequence uint64) string {
	return path.Join(AckDir(transmissionID), fmt.Sprintf(".%020d.json.tmp", sequence))
}

// Result is the receiver's final word on a transmission, written to the acknowledgement channel
// once the file has been merged and checked.
//
// It exists because of where the callback comes from. The callback URL arrives on the *sender's*
// API, with the file — so the sender is the side that must eventually make the call. But the
// sender cannot know whether the transfer actually worked: acknowledgements tell it every chunk
// arrived, which is not the same as the file being right. Only the receiver can compare the
// merged file against the hash the manifest declared.
//
// So the receiver reports its verdict back through the channel that already exists, signed with
// the same secret, and the sender includes it in the callback. That keeps the optical link
// one-way, adds no second network path, and means the callback says "arrived and verified,
// here is the hash" rather than "we sent everything we had".
type Result struct {
	TransmissionID uuid.UUID `json:"transmission_id"`

	// Filename, Size, and SHA256 describe the file as the receiver merged it. The hash is the
	// figure that matters: it is compared against what the sender's manifest declared, so the
	// two ends agree on success rather than each asserting it separately.
	Filename string `json:"filename"`
	Size     uint64 `json:"size"`
	SHA256   string `json:"sha256"`

	// Verified is whether the merged file matched the manifest, and Error says why not.
	Verified bool   `json:"verified"`
	Error    string `json:"error,omitempty"`

	// ChunksExpected and ChunksReceived quantify the transfer, and ChunksRecovered how many of
	// them came from parity rather than from a frame that arrived. A non-zero recovery count is
	// what tells an operator the error correction is earning its channel time.
	ChunksExpected  uint32 `json:"chunks_expected"`
	ChunksReceived  uint32 `json:"chunks_received"`
	ChunksRecovered uint32 `json:"chunks_recovered"`

	// FramesCaptured and FramesFailed describe the optical channel itself: how many frames the
	// camera saw and how many of those were unreadable.
	FramesCaptured uint64 `json:"frames_captured"`
	FramesFailed   uint64 `json:"frames_failed"`

	// StartedMS and CompletedMS bound the transfer, in Unix milliseconds, so throughput can be
	// computed from the record rather than estimated.
	StartedMS   uint64 `json:"started_ms"`
	CompletedMS uint64 `json:"completed_ms"`

	// CallbackURL is where the receiver delivered the merged file, and CallbackDelivered and
	// CallbackStatus are what came of it.
	//
	// These are what close the loop the caller started. Somebody handed the sender a file and a
	// URL; the file crossed the optical gap, was reassembled, verified, and posted to that URL —
	// and this is how the sender finds out, since it has no other view of what happened on the
	// far side. A transfer whose chunks all arrived but whose delivery failed is not a success,
	// and without these fields the sender could not tell the two apart.
	CallbackURL       string `json:"callback_url,omitempty"`
	CallbackDelivered bool   `json:"callback_delivered"`
	CallbackStatus    int    `json:"callback_status,omitempty"`
	CallbackError     string `json:"callback_error,omitempty"`
}

// Duration is how long the transfer took.
func (r Result) Duration() time.Duration {
	if r.CompletedMS <= r.StartedMS {
		return 0
	}
	return time.Duration(r.CompletedMS-r.StartedMS) * time.Millisecond
}

// ThroughputBytesPerSecond is the rate the file arrived at, or zero if it cannot be computed.
func (r Result) ThroughputBytesPerSecond() float64 {
	seconds := r.Duration().Seconds()
	if seconds <= 0 {
		return 0
	}
	return float64(r.Size) / seconds
}

// Validate checks a result describes something that could have happened.
func (r Result) Validate() error {
	if r.TransmissionID == uuid.Nil {
		return fmt.Errorf("%w: no transmission id", ErrAckMalformed)
	}
	if err := checkFilename(r.Filename); err != nil {
		return err
	}
	if r.Verified && len(r.SHA256) != 64 {
		return fmt.Errorf("%w: a verified result needs a hash", ErrAckMalformed)
	}
	if r.ChunksReceived > r.ChunksExpected && r.ChunksExpected > 0 {
		return fmt.Errorf("%w: %d chunks received of %d expected",
			ErrAckMalformed, r.ChunksReceived, r.ChunksExpected)
	}
	if err := CheckCallbackURL(r.CallbackURL); err != nil {
		return err
	}
	if r.CallbackDelivered && r.CallbackURL == "" {
		return fmt.Errorf("%w: a delivered callback needs a URL", ErrAckMalformed)
	}
	return nil
}

// SignResult serialises and signs a result, in the same envelope acknowledgements use.
func SignResult(secret []byte, r Result) ([]byte, error) {
	if len(secret) == 0 {
		return nil, fmt.Errorf("%w: no signing secret", ErrAckSignature)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}

	record, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(record)

	return json.Marshal(SignedAck{
		Record:    record,
		Signature: hex.EncodeToString(mac.Sum(nil)),
	})
}

// ParseResult verifies a signed result and returns it.
//
// The signature matters more here than anywhere else in the protocol, because this record is what
// the sender turns into a callback: an unsigned result would let anything able to write the
// acknowledgement directory make the sender report a transfer as verified when it was not.
func ParseResult(secret, data []byte) (Result, error) {
	if len(secret) == 0 {
		return Result{}, fmt.Errorf("%w: no signing secret", ErrAckSignature)
	}

	var signed SignedAck
	if err := json.Unmarshal(data, &signed); err != nil {
		return Result{}, fmt.Errorf("%w: %s", ErrAckMalformed, err)
	}
	if len(signed.Record) == 0 {
		return Result{}, fmt.Errorf("%w: no record", ErrAckMalformed)
	}

	want, err := hex.DecodeString(signed.Signature)
	if err != nil {
		return Result{}, fmt.Errorf("%w: signature is not hex", ErrAckSignature)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(signed.Record)
	if !hmac.Equal(mac.Sum(nil), want) {
		return Result{}, ErrAckSignature
	}

	var r Result
	if err := json.Unmarshal(signed.Record, &r); err != nil {
		return Result{}, fmt.Errorf("%w: %s", ErrAckMalformed, err)
	}
	if err := r.Validate(); err != nil {
		return Result{}, err
	}
	return r, nil
}

// ResultPath is where the receiver's verdict is written.
//
// It sits beside the acknowledgements rather than in the sequence-numbered namespace, so the
// sender's watcher can tell a per-chunk report from the final verdict by name alone and does not
// have to parse every record to find out which it has.
func ResultPath(transmissionID uuid.UUID) string {
	return path.Join(AckDir(transmissionID), "result.json")
}

// ResultTempPath is the name a result is written under before being renamed into place.
func ResultTempPath(transmissionID uuid.UUID) string {
	return path.Join(AckDir(transmissionID), ".result.json.tmp")
}
