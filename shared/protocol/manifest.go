package protocol

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Manifest describes a whole transmission, and travels as the payload of the
// frames that set FlagManifest.
//
// It exists because a receiver cannot learn any of this from a data frame. A frame
// header says which chunk it carries and how many there are, but not what the
// chunks reassemble into, how to decompress them, how they were error-coded, or
// what the finished file should hash to. Without that, a receiver can collect every
// chunk of a transmission and still have nothing it can write to disk.
//
// The sender re-emits it periodically rather than only at the start, so a receiver
// whose camera came online mid-transmission joins the stream instead of waiting for
// the next one — which, for a file that takes an hour to display, is the difference
// between a working installation and an unusable one.
type Manifest struct {
	// Filename is the original name, without any directory part.
	Filename string `json:"filename"`

	// OriginalSize and OriginalSHA256 describe the file as the sender received it,
	// before compression. They are what the receiver verifies the merged file
	// against, so they are the transmission's ultimate success criterion.
	OriginalSize   uint64   `json:"original_size"`
	OriginalSHA256 [32]byte `json:"original_sha256"`

	// CompressedSize is the size of the byte stream that was actually chunked.
	CompressedSize uint64 `json:"compressed_size"`

	// ChunkCount and ChunkSize describe the split. The final chunk may be shorter.
	ChunkCount uint32 `json:"chunk_count"`
	ChunkSize  uint32 `json:"chunk_size"`

	// CompressionID selects the compressor the receiver must run in reverse.
	CompressionID uint8 `json:"compression_id"`

	// FEC describes the error-correction the sender applied.
	FEC FECParams `json:"fec"`

	// CallbackURL is where the receiver delivers the merged file once it has verified it.
	//
	// It travels in the manifest because of who has it and who needs it. The URL is supplied on
	// the *sender's* API, with the file — that is where a caller says where the result should go
	// — but the receiver is the side that ends up holding a merged, verified file to deliver. The
	// optical link is the only path between them, so the URL rides across it.
	//
	// A receiver must treat it as hostile input. It arrives from outside the receiver's trust
	// boundary and is then used to make an outbound request, which is a server-side request
	// forgery waiting to happen: a URL naming an internal address would have the receiver fetch
	// or post to somewhere its operator never intended. CheckCallbackURL rejects the shapes that
	// cannot be legitimate, and a receiver is expected to hold an allowlist of hosts on top of
	// that. Empty means no callback.
	CallbackURL string `json:"callback_url,omitempty"`

	// Reserved carries forward compatibility, like the header's.
	Reserved [8]byte `json:"-"`
}

// FECParams is the error-correction geometry, in terms general enough for every
// codec the platform offers.
//
// The four codecs are described by the same three numbers despite working very
// differently. Reed-Solomon and LDPC are block codes: DataShards source shards
// produce ParityShards parity shards, and any DataShards of the total reconstruct
// the block. RaptorQ is a fountain code with no fixed parity count, so
// ParityShards is how many repair symbols the sender chose to emit — a decoder
// needs only slightly more than DataShards symbols of any kind, which is why it
// tolerates losing whichever ones it lost.
type FECParams struct {
	// ID selects the codec. Zero is the no-op codec.
	ID uint8 `json:"id"`

	// DataShards is the number of source shards per block, and ParityShards the
	// number of recovery shards generated alongside them.
	DataShards   uint16 `json:"data_shards"`
	ParityShards uint16 `json:"parity_shards"`

	// ShardSize is the length of one shard, in bytes.
	ShardSize uint32 `json:"shard_size"`
}

// Manifest wire limits.
const (
	// MaxFilenameBytes bounds the filename field.
	MaxFilenameBytes = 255

	// MaxCallbackURLBytes bounds the callback URL. It is generous for a real URL and small
	// enough that a manifest still fits one frame at a modest grid — the manifest is the first
	// frame of every transmission, so it has to fit wherever the payload does.
	MaxCallbackURLBytes = 512

	// manifestFixedSize is the serialised size of everything but the two variable-length fields.
	manifestFixedSize = 4 + 2 + 1 + 2 + 8 + 32 + 8 + 4 + 4 + 1 + 1 + 2 + 2 + 4 + 8 + 4
)

var manifestMagic = [4]byte{'O', 'T', 'P', 'M'}

// Manifest errors.
var (
	// ErrBadFilename means the filename is empty, over-long, not valid UTF-8, or
	// carries a directory component.
	ErrBadFilename = fmt.Errorf("protocol: invalid manifest filename")

	// ErrNotManifest means a frame was asked for its manifest without carrying one.
	ErrNotManifest = fmt.Errorf("protocol: frame does not carry a manifest")

	// ErrManifestCRC means the manifest record failed its checksum.
	ErrManifestCRC = fmt.Errorf("protocol: manifest CRC mismatch")

	// ErrManifestInconsistent means the manifest's own fields contradict each other.
	ErrManifestInconsistent = fmt.Errorf("protocol: manifest fields are inconsistent")

	// ErrBadCallbackURL means the callback URL is not one a receiver should act on.
	ErrBadCallbackURL = fmt.Errorf("protocol: invalid callback URL")
)

// Size is the serialised length of this manifest.
func (m Manifest) Size() int {
	return manifestFixedSize + len(m.Filename) + len(m.CallbackURL)
}

// CheckManifestFilename rejects anything that must not reach a receiver's filesystem.
//
// It is exported because the sender's API needs the same judgement at the moment a caller supplies a
// name, rather than discovering the problem when the manifest is built — by which time the upload has
// been read and the caller has been told it was accepted.
func CheckManifestFilename(name string) error { return checkFilename(name) }

// checkFilename rejects anything that must not reach a receiver's filesystem.
//
// The receiver writes the merged file under this name, and the name arrives over
// an optical channel from outside its trust boundary. A name carrying a directory
// component or a parent reference would let a sender choose where on the receiver
// the file lands, which is a path-traversal write; rejecting it here means every
// consumer of a Manifest is holding a name that has already been checked, rather
// than each of them having to remember to check it.
func checkFilename(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: empty", ErrBadFilename)
	case len(name) > MaxFilenameBytes:
		return fmt.Errorf("%w: %d bytes exceeds the %d-byte field", ErrBadFilename, len(name), MaxFilenameBytes)
	case !utf8.ValidString(name):
		return fmt.Errorf("%w: not valid UTF-8", ErrBadFilename)
	case name == "." || name == "..":
		return fmt.Errorf("%w: %q is a directory reference", ErrBadFilename, name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("%w: %q carries a directory component", ErrBadFilename, name)
	}
	for _, r := range name {
		// Control characters and NUL have no place in a filename and are a classic
		// way to make one thing look like another in a log or a UI.
		if r < 0x20 || r == 0x7F {
			return fmt.Errorf("%w: %q contains a control character", ErrBadFilename, name)
		}
	}
	return nil
}

// CheckCallbackURL rejects callback URLs a receiver must not act on.
//
// The URL crossed the optical channel, so it is attacker-influenced input that will be turned
// into an outbound request. What is checked here is only what can be judged from the string: the
// scheme has to be one an HTTP client should speak, and the URL has to name a host. It cannot
// judge *where* that host is, which is the more dangerous question — a URL naming an address
// inside the receiver's network passes every test here. That is why a receiver is expected to
// hold an allowlist of hosts as well, and why this function is not the whole defence.
func CheckCallbackURL(raw string) error {
	if raw == "" {
		return nil
	}
	if len(raw) > MaxCallbackURLBytes {
		return fmt.Errorf("%w: %d bytes exceeds the %d-byte field",
			ErrBadCallbackURL, len(raw), MaxCallbackURLBytes)
	}
	if !utf8.ValidString(raw) {
		return fmt.Errorf("%w: not valid UTF-8", ErrBadCallbackURL)
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7F {
			// A control character in a URL is how a request gets split into two.
			return fmt.Errorf("%w: contains a control character", ErrBadCallbackURL)
		}
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrBadCallbackURL, err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("%w: scheme %q is not http or https", ErrBadCallbackURL, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: %q names no host", ErrBadCallbackURL, raw)
	}
	if u.User != nil {
		// Credentials in a URL would be sent by the receiver on its operator's behalf to a host
		// the operator did not choose.
		return fmt.Errorf("%w: %q carries credentials", ErrBadCallbackURL, raw)
	}
	return nil
}

// Validate checks the manifest describes a transmission that could exist.
func (m Manifest) Validate() error {
	if err := checkFilename(m.Filename); err != nil {
		return err
	}
	if m.ChunkCount == 0 || m.ChunkSize == 0 {
		return fmt.Errorf("%w: %d chunks of %d bytes", ErrManifestInconsistent, m.ChunkCount, m.ChunkSize)
	}

	if m.CompressedSize == 0 {
		// An empty file is a legitimate thing to send, and it is one empty chunk.
		if m.OriginalSize > 0 {
			return fmt.Errorf("%w: %d original bytes compressed to nothing",
				ErrManifestInconsistent, m.OriginalSize)
		}
		if m.ChunkCount != 1 {
			return fmt.Errorf("%w: an empty transmission is one chunk, not %d",
				ErrManifestInconsistent, m.ChunkCount)
		}
		return nil
	}

	// Otherwise the chunking has to account for exactly the compressed stream:
	// enough chunks to hold it, and not so many that one of them would be empty. A
	// receiver that trusted a manifest failing this would either truncate the file
	// or wait forever for a chunk the sender never sent.
	span := uint64(m.ChunkCount) * uint64(m.ChunkSize)
	if span < m.CompressedSize || span-uint64(m.ChunkSize) >= m.CompressedSize {
		return fmt.Errorf("%w: %d chunks of %d bytes cannot hold %d compressed bytes",
			ErrManifestInconsistent, m.ChunkCount, m.ChunkSize, m.CompressedSize)
	}
	if err := CheckCallbackURL(m.CallbackURL); err != nil {
		return err
	}
	if m.FEC.ID != 0 && m.FEC.DataShards == 0 {
		return fmt.Errorf("%w: FEC codec %d with no source shards", ErrManifestInconsistent, m.FEC.ID)
	}
	return nil
}

// MarshalBinary serialises the manifest and appends its CRC32.
func (m Manifest) MarshalBinary() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}

	b := make([]byte, m.Size())
	copy(b[0:4], manifestMagic[:])
	binary.BigEndian.PutUint16(b[4:6], Current)
	b[6] = uint8(len(m.Filename))
	binary.BigEndian.PutUint16(b[7:9], uint16(len(m.CallbackURL)))
	binary.BigEndian.PutUint64(b[9:17], m.OriginalSize)
	copy(b[17:49], m.OriginalSHA256[:])
	binary.BigEndian.PutUint64(b[49:57], m.CompressedSize)
	binary.BigEndian.PutUint32(b[57:61], m.ChunkCount)
	binary.BigEndian.PutUint32(b[61:65], m.ChunkSize)
	b[65] = m.CompressionID
	b[66] = m.FEC.ID
	binary.BigEndian.PutUint16(b[67:69], m.FEC.DataShards)
	binary.BigEndian.PutUint16(b[69:71], m.FEC.ParityShards)
	binary.BigEndian.PutUint32(b[71:75], m.FEC.ShardSize)
	copy(b[75:83], m.Reserved[:])
	copy(b[83:83+len(m.Filename)], m.Filename)
	copy(b[83+len(m.Filename):83+len(m.Filename)+len(m.CallbackURL)], m.CallbackURL)
	binary.BigEndian.PutUint32(b[m.Size()-4:], crc32.ChecksumIEEE(b[:m.Size()-4]))
	return b, nil
}

// UnmarshalBinary parses a manifest and verifies its magic, version, checksum, and
// internal consistency.
func (m *Manifest) UnmarshalBinary(b []byte) error {
	if len(b) < manifestFixedSize {
		return fmt.Errorf("%w: manifest needs at least %d bytes, got %d",
			ErrShortBuffer, manifestFixedSize, len(b))
	}
	if [4]byte(b[0:4]) != manifestMagic {
		return fmt.Errorf("%w: manifest magic %q", ErrBadMagic, b[0:4])
	}
	if err := CheckVersion(binary.BigEndian.Uint16(b[4:6])); err != nil {
		return err
	}

	nameLen := int(b[6])
	urlLen := int(binary.BigEndian.Uint16(b[7:9]))
	size := manifestFixedSize + nameLen + urlLen
	if len(b) < size {
		return fmt.Errorf("%w: manifest declares a %d-byte name and a %d-byte URL, needing %d bytes, got %d",
			ErrShortBuffer, nameLen, urlLen, size, len(b))
	}

	// The record is checksummed at its declared length rather than the whole
	// buffer, because a manifest arrives as a frame payload that may be padded.
	want := binary.BigEndian.Uint32(b[size-4 : size])
	if got := crc32.ChecksumIEEE(b[:size-4]); got != want {
		return fmt.Errorf("%w: computed %08x, manifest declares %08x", ErrManifestCRC, got, want)
	}

	m.OriginalSize = binary.BigEndian.Uint64(b[9:17])
	copy(m.OriginalSHA256[:], b[17:49])
	m.CompressedSize = binary.BigEndian.Uint64(b[49:57])
	m.ChunkCount = binary.BigEndian.Uint32(b[57:61])
	m.ChunkSize = binary.BigEndian.Uint32(b[61:65])
	m.CompressionID = b[65]
	m.FEC = FECParams{
		ID:           b[66],
		DataShards:   binary.BigEndian.Uint16(b[67:69]),
		ParityShards: binary.BigEndian.Uint16(b[69:71]),
		ShardSize:    binary.BigEndian.Uint32(b[71:75]),
	}
	copy(m.Reserved[:], b[75:83])
	m.Filename = string(b[83 : 83+nameLen])
	m.CallbackURL = string(b[83+nameLen : 83+nameLen+urlLen])

	// Validation runs on the way in as well as out. A manifest that passed its CRC
	// is intact, not trustworthy: it was authored by whatever was on the other end
	// of the optical channel, and the filename in it is about to be used.
	return m.Validate()
}

// String renders a manifest for logs and the transmission UI.
func (m Manifest) String() string {
	callback := "no callback"
	if m.CallbackURL != "" {
		callback = "callback " + m.CallbackURL
	}
	return fmt.Sprintf("%q %d bytes -> %d compressed in %d chunks of %d (compression %d, fec %d %d+%d, %s)",
		m.Filename, m.OriginalSize, m.CompressedSize, m.ChunkCount, m.ChunkSize,
		m.CompressionID, m.FEC.ID, m.FEC.DataShards, m.FEC.ParityShards, callback)
}

// NewManifestFrame builds the frame that carries a manifest.
//
// The flag and the payload are set together here so they cannot disagree. A frame
// carrying manifest bytes without the flag would be reassembled into the file as
// though it were data, which corrupts the output in a way no checksum catches
// until the very end.
func NewManifestFrame(h Header, m Manifest) (*Frame, error) {
	payload, err := m.MarshalBinary()
	if err != nil {
		return nil, err
	}
	h.Flags |= FlagManifest
	h.CompressionID = m.CompressionID
	h.FECID = m.FEC.ID
	h.TotalChunks = m.ChunkCount
	return NewFrame(h, payload), nil
}

// ParseManifest reads the manifest a frame carries.
func ParseManifest(f *Frame) (Manifest, error) {
	if f == nil {
		return Manifest{}, ErrNotManifest
	}
	if !f.Header.Flags.Has(FlagManifest) {
		return Manifest{}, fmt.Errorf("%w: flags are [%s]", ErrNotManifest, f.Header.Flags)
	}
	var m Manifest
	if err := m.UnmarshalBinary(f.Payload); err != nil {
		return Manifest{}, err
	}
	return m, nil
}
