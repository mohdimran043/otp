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
