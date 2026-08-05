package protocol

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"time"

	"github.com/google/uuid"
)

// Wire sizes of the fixed records, in bytes. These are asserted by test to keep
// the constants honest against the marshalling code.
const (
	// HeaderSize is the serialised size of a Header, including its CRC.
	HeaderSize = 92
	// FooterSize is the serialised size of a Footer, including its CRC.
	FooterSize = 52
)

// Magic values identifying the two fixed bands.
var (
	headerMagic = [4]byte{'O', 'T', 'P', '1'}
	footerMagic = [4]byte{'O', 'T', 'P', 'F'}
)

// Flags carries per-frame boolean state.
type Flags uint16

// Frame flags.
const (
	// FlagManifest marks the frame whose payload is a Manifest rather than file
	// data. Frame 0 always sets it, and it is re-emitted periodically so a
	// receiver can join a transmission already in progress.
	FlagManifest Flags = 1 << 0

	// FlagParity marks a frame carrying an error-correction parity chunk rather
	// than a source chunk.
	FlagParity Flags = 1 << 1

	// FlagEncrypted marks a payload encrypted with AES-256-GCM.
	FlagEncrypted Flags = 1 << 2

	// FlagLastChunk marks the final source chunk of a transmission.
	FlagLastChunk Flags = 1 << 3

	// FlagRetransmit marks a frame the scheduler is resending because the
	// receiver did not acknowledge it.
	FlagRetransmit Flags = 1 << 4

	// FlagKeepAlive marks a frame displayed only to keep the optical channel
	// active while the send window is saturated. The sender never blanks the
	// display, so there is always something for the camera to lock onto.
	FlagKeepAlive Flags = 1 << 5

	// FlagEndOfStream marks the final frame of a transmission.
	FlagEndOfStream Flags = 1 << 6
)

// Has reports whether all bits in q are set.
func (f Flags) Has(q Flags) bool { return f&q == q }

// String renders the set flags for logs and the debugging UI.
func (f Flags) String() string {
	names := []struct {
		bit  Flags
		name string
	}{
		{FlagManifest, "manifest"},
		{FlagParity, "parity"},
		{FlagEncrypted, "encrypted"},
		{FlagLastChunk, "last-chunk"},
		{FlagRetransmit, "retransmit"},
		{FlagKeepAlive, "keep-alive"},
		{FlagEndOfStream, "end-of-stream"},
	}
	out := make([]byte, 0, 48)
	for _, n := range names {
		if !f.Has(n.bit) {
			continue
		}
		if len(out) > 0 {
			out = append(out, '|')
		}
		out = append(out, n.name...)
	}
	if len(out) == 0 {
		return "none"
	}
	return string(out)
}

// Header is the fixed record written into the top band of every frame.
//
// It is always modulated in plain binary with its own checksum, whatever
// modulation the payload uses. That is the property that makes the grid
// adaptive: a decoder holding an unfamiliar frame can always read the header,
// learn the geometry, bit depth, and encoder identity, and only then demodulate
// the payload.
type Header struct {
	Version        uint16    `json:"version"`
	Flags          Flags     `json:"flags"`
	EncoderID      uint8     `json:"encoder_id"`
	BitDepth       uint8     `json:"bit_depth"`
	CompressionID  uint8     `json:"compression_id"`
	FECID          uint8     `json:"fec_id"`
	CellPixels     uint16    `json:"cell_pixels"`
	GridWidth      uint16    `json:"grid_width"`
	GridHeight     uint16    `json:"grid_height"`
	TransmissionID uuid.UUID `json:"transmission_id"`
	SessionID      uuid.UUID `json:"session_id"`
	FrameNumber    uint32    `json:"frame_number"`
	TotalFrames    uint32    `json:"total_frames"`
	ChunkNumber    uint32    `json:"chunk_number"`
	TotalChunks    uint32    `json:"total_chunks"`
	PayloadLength  uint32    `json:"payload_length"`
	TimestampMS    uint64    `json:"timestamp_ms"`
	Reserved       [8]byte   `json:"-"`
}

// Timestamp returns the header time as a Go time value.
func (h Header) Timestamp() time.Time {
	return time.UnixMilli(int64(h.TimestampMS)).UTC()
}

// MarshalBinary serialises the header and appends its CRC32.
func (h Header) MarshalBinary() ([]byte, error) {
	b := make([]byte, HeaderSize)
	copy(b[0:4], headerMagic[:])
	binary.BigEndian.PutUint16(b[4:6], h.Version)
	binary.BigEndian.PutUint16(b[6:8], HeaderSize)
	binary.BigEndian.PutUint16(b[8:10], uint16(h.Flags))
	b[10] = h.EncoderID
	b[11] = h.BitDepth
	b[12] = h.CompressionID
	b[13] = h.FECID
	binary.BigEndian.PutUint16(b[14:16], h.CellPixels)
	binary.BigEndian.PutUint16(b[16:18], h.GridWidth)
	binary.BigEndian.PutUint16(b[18:20], h.GridHeight)
	copy(b[20:36], h.TransmissionID[:])
	copy(b[36:52], h.SessionID[:])
	binary.BigEndian.PutUint32(b[52:56], h.FrameNumber)
	binary.BigEndian.PutUint32(b[56:60], h.TotalFrames)
	binary.BigEndian.PutUint32(b[60:64], h.ChunkNumber)
	binary.BigEndian.PutUint32(b[64:68], h.TotalChunks)
	binary.BigEndian.PutUint32(b[68:72], h.PayloadLength)
	binary.BigEndian.PutUint64(b[72:80], h.TimestampMS)
	copy(b[80:88], h.Reserved[:])
	binary.BigEndian.PutUint32(b[88:92], crc32.ChecksumIEEE(b[0:88]))
	return b, nil
}

// UnmarshalBinary parses a header and verifies its magic, CRC, and version.
//
// Unknown reserved bytes are preserved rather than rejected, so a frame produced
// by a future build that only added reserved-field meaning still decodes here.
func (h *Header) UnmarshalBinary(b []byte) error {
	if len(b) < HeaderSize {
		return fmt.Errorf("%w: header needs %d bytes, got %d", ErrShortBuffer, HeaderSize, len(b))
	}
	if [4]byte(b[0:4]) != headerMagic {
		return fmt.Errorf("%w: header magic %q", ErrBadMagic, b[0:4])
	}
	want := binary.BigEndian.Uint32(b[88:92])
	if got := crc32.ChecksumIEEE(b[0:88]); got != want {
		return fmt.Errorf("%w: computed %08x, header declares %08x", ErrHeaderCRC, got, want)
	}
	h.Version = binary.BigEndian.Uint16(b[4:6])
	if err := CheckVersion(h.Version); err != nil {
		return err
	}
	h.Flags = Flags(binary.BigEndian.Uint16(b[8:10]))
	h.EncoderID = b[10]
	h.BitDepth = b[11]
	h.CompressionID = b[12]
	h.FECID = b[13]
	h.CellPixels = binary.BigEndian.Uint16(b[14:16])
	h.GridWidth = binary.BigEndian.Uint16(b[16:18])
	h.GridHeight = binary.BigEndian.Uint16(b[18:20])
	h.TransmissionID = uuid.UUID(b[20:36])
	h.SessionID = uuid.UUID(b[36:52])
	h.FrameNumber = binary.BigEndian.Uint32(b[52:56])
	h.TotalFrames = binary.BigEndian.Uint32(b[56:60])
	h.ChunkNumber = binary.BigEndian.Uint32(b[60:64])
	h.TotalChunks = binary.BigEndian.Uint32(b[64:68])
	h.PayloadLength = binary.BigEndian.Uint32(b[68:72])
	h.TimestampMS = binary.BigEndian.Uint64(b[72:80])
	copy(h.Reserved[:], b[80:88])
	return nil
}

// Footer is the fixed record written into the bottom band of every frame. It
// carries the payload integrity values, so a receiver can reject a corrupt
// frame without waiting for the whole file to be reassembled.
type Footer struct {
	PayloadCRC32  uint32   `json:"payload_crc32"`
	PayloadSHA256 [32]byte `json:"payload_sha256"`
	Reserved      [8]byte  `json:"-"`
}

// MarshalBinary serialises the footer and appends its CRC32.
func (f Footer) MarshalBinary() ([]byte, error) {
	b := make([]byte, FooterSize)
	binary.BigEndian.PutUint32(b[0:4], f.PayloadCRC32)
	copy(b[4:36], f.PayloadSHA256[:])
	copy(b[36:44], f.Reserved[:])
	copy(b[44:48], footerMagic[:])
	binary.BigEndian.PutUint32(b[48:52], crc32.ChecksumIEEE(b[0:48]))
	return b, nil
}

// UnmarshalBinary parses a footer and verifies its magic and CRC.
func (f *Footer) UnmarshalBinary(b []byte) error {
	if len(b) < FooterSize {
		return fmt.Errorf("%w: footer needs %d bytes, got %d", ErrShortBuffer, FooterSize, len(b))
	}
	if [4]byte(b[44:48]) != footerMagic {
		return fmt.Errorf("%w: footer magic %q", ErrBadMagic, b[44:48])
	}
	want := binary.BigEndian.Uint32(b[48:52])
	if got := crc32.ChecksumIEEE(b[0:48]); got != want {
		return fmt.Errorf("%w: computed %08x, footer declares %08x", ErrFooterCRC, got, want)
	}
	f.PayloadCRC32 = binary.BigEndian.Uint32(b[0:4])
	copy(f.PayloadSHA256[:], b[4:36])
	copy(f.Reserved[:], b[36:44])
	return nil
}

// Frame is one displayable unit of the protocol: a header, a payload, and a
// footer that authenticates the payload.
type Frame struct {
	Header  Header
	Payload []byte
	Footer  Footer
}

// NewFrame builds a frame around a payload, filling in the derived fields:
// payload length, timestamp, CRC32, and SHA-256.
func NewFrame(h Header, payload []byte) *Frame {
	h.Version = Current
	h.PayloadLength = uint32(len(payload))
	if h.TimestampMS == 0 {
		h.TimestampMS = uint64(time.Now().UTC().UnixMilli())
	}
	return &Frame{
		Header:  h,
		Payload: payload,
		Footer: Footer{
			PayloadCRC32:  crc32.ChecksumIEEE(payload),
			PayloadSHA256: sha256.Sum256(payload),
		},
	}
}

// Verify checks the payload against both integrity values in the footer.
//
// CRC32 is checked first because it is the cheaper test and catches the
// overwhelming majority of optical bit errors; SHA-256 then rules out the
// collisions CRC32 cannot.
func (f *Frame) Verify() error {
	if len(f.Payload) != int(f.Header.PayloadLength) {
		return fmt.Errorf("%w: header declares %d payload bytes, frame carries %d",
			ErrPayloadCRC, f.Header.PayloadLength, len(f.Payload))
	}
	if got := crc32.ChecksumIEEE(f.Payload); got != f.Footer.PayloadCRC32 {
		return fmt.Errorf("%w: computed %08x, footer declares %08x",
			ErrPayloadCRC, got, f.Footer.PayloadCRC32)
	}
	if got := sha256.Sum256(f.Payload); got != f.Footer.PayloadSHA256 {
		return ErrPayloadHash
	}
	return nil
}

// String renders a frame identity for logs.
func (f *Frame) String() string {
	return fmt.Sprintf("frame %d/%d chunk %d/%d tx=%s %d bytes [%s]",
		f.Header.FrameNumber, f.Header.TotalFrames,
		f.Header.ChunkNumber, f.Header.TotalChunks,
		f.Header.TransmissionID, len(f.Payload), f.Header.Flags)
}
