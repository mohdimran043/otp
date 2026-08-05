package protocol_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/shared/protocol"
)

func sampleAck() protocol.Ack {
	return protocol.Ack{
		Sequence:       417,
		TransmissionID: uuid.MustParse("6f9619ff-8b86-d011-b42d-00cf4fc964ff"),
		SessionID:      uuid.MustParse("9c1e5f42-0d3a-4b21-8f77-2b1d5a6c9e01"),
		FrameNumber:    1204,
		ChunkNumber:    1198,
		Status:         protocol.AckOK,
		RetryCount:     2,
		TimestampMS:    1754300000000,
		BitErrorRate:   0.0031,
	}
}

func ackSecret() []byte { return []byte("shared acknowledgement secret") }

func TestAckRoundTrips(t *testing.T) {
	want := sampleAck()

	data, err := protocol.SignAck(ackSecret(), want)
	require.NoError(t, err)

	got, err := protocol.ParseAck(ackSecret(), data)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// TestAckSignatureCoversTheExactBytes is the reason the record travels as raw JSON.
// If verification re-serialised a parsed record instead, the signature would depend on
// this program's JSON encoder, and a Go upgrade on one side of the air gap would make
// every acknowledgement fail to verify on the other.
func TestAckSignatureCoversTheExactBytes(t *testing.T) {
	data, err := protocol.SignAck(ackSecret(), sampleAck())
	require.NoError(t, err)

	var signed protocol.SignedAck
	require.NoError(t, json.Unmarshal(data, &signed))
	require.NotEmpty(t, signed.Signature)

	// Reformatting the record without changing its meaning must invalidate the
	// signature, because the signature is over bytes rather than over meaning.
	var asMap map[string]any
	require.NoError(t, json.Unmarshal(signed.Record, &asMap))
	reformatted, err := json.MarshalIndent(asMap, "", "  ")
	require.NoError(t, err)
	require.NotEqual(t, string(signed.Record), string(reformatted))

	tampered, err := json.Marshal(protocol.SignedAck{
		Record:    reformatted,
		Signature: signed.Signature,
	})
	require.NoError(t, err)
	_, err = protocol.ParseAck(ackSecret(), tampered)
	require.ErrorIs(t, err, protocol.ErrAckSignature)
}

// TestAckForgeryIsRefused is a security test. The acknowledgement channel is the one
// input a sender takes from outside itself: anything able to write to that directory
// could otherwise report chunks as delivered when they were not, truncating the
// transmission, or report every chunk as failed, making the sender retransmit for ever.
func TestAckForgeryIsRefused(t *testing.T) {
	data, err := protocol.SignAck(ackSecret(), sampleAck())
	require.NoError(t, err)

	t.Run("a record with no signature", func(t *testing.T) {
		forged, err := json.Marshal(protocol.SignedAck{Record: json.RawMessage(`{"status":"ok"}`)})
		require.NoError(t, err)
		_, err = protocol.ParseAck(ackSecret(), forged)
		require.ErrorIs(t, err, protocol.ErrAckSignature)
	})

	t.Run("the wrong secret", func(t *testing.T) {
		_, err := protocol.ParseAck([]byte("a different secret"), data)
		require.ErrorIs(t, err, protocol.ErrAckSignature)
	})

	t.Run("a flipped status", func(t *testing.T) {
		var signed protocol.SignedAck
		require.NoError(t, json.Unmarshal(data, &signed))

		record := string(signed.Record)
		altered := []byte(replace(record, `"status":"ok"`, `"status":"crc_failed"`))
		require.NotEqual(t, record, string(altered), "the test must actually change the record")

		forged, err := json.Marshal(protocol.SignedAck{Record: altered, Signature: signed.Signature})
		require.NoError(t, err)
		_, err = protocol.ParseAck(ackSecret(), forged)
		require.ErrorIs(t, err, protocol.ErrAckSignature)
	})

	t.Run("a truncated file", func(t *testing.T) {
		// What a sender reads if it polls a directory while the receiver is mid-write,
		// which is why records are written under a temporary name and renamed.
		_, err := protocol.ParseAck(ackSecret(), data[:len(data)/2])
		require.ErrorIs(t, err, protocol.ErrAckMalformed)
	})

	t.Run("no secret configured", func(t *testing.T) {
		_, err := protocol.ParseAck(nil, data)
		require.ErrorIs(t, err, protocol.ErrAckSignature)
		_, err = protocol.SignAck(nil, sampleAck())
		require.ErrorIs(t, err, protocol.ErrAckSignature)
	})
}

func replace(s, old, new string) string {
	i := indexOf(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + new + s[i+len(old):]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestAckValidationRejectsImpossibleRecords(t *testing.T) {
	cases := map[string]func(*protocol.Ack){
		"unknown status":       func(a *protocol.Ack) { a.Status = "maybe" },
		"empty status":         func(a *protocol.Ack) { a.Status = "" },
		"no transmission":      func(a *protocol.Ack) { a.TransmissionID = uuid.Nil },
		"no timestamp":         func(a *protocol.Ack) { a.TimestampMS = 0 },
		"negative error rate":  func(a *protocol.Ack) { a.BitErrorRate = -0.1 },
		"error rate above one": func(a *protocol.Ack) { a.BitErrorRate = 1.5 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a := sampleAck()
			mutate(&a)
			require.Error(t, a.Validate())

			// A record that cannot be valid must not be signable either, so a broken
			// receiver cannot produce records a sender will act on.
			_, err := protocol.SignAck(ackSecret(), a)
			require.ErrorIs(t, err, protocol.ErrAckMalformed)
		})
	}
}

// TestDeliveredStatuses pins which outcomes stop retransmission. Getting this wrong in
// either direction is costly: too strict and the sender resends chunks the receiver
// already holds for ever, too loose and it drops chunks that never arrived.
func TestDeliveredStatuses(t *testing.T) {
	for status, delivered := range map[protocol.AckStatus]bool{
		protocol.AckOK:           true,
		protocol.AckDuplicate:    true,
		protocol.AckRecovered:    true,
		protocol.AckCRCFailed:    false,
		protocol.AckDecodeFailed: false,
	} {
		require.True(t, status.Valid(), "%s", status)
		require.Equal(t, delivered, status.Delivered(), "%s", status)
	}

	require.False(t, protocol.AckStatus("invented").Valid())
	require.False(t, protocol.AckStatus("invented").Delivered())
}

// TestAckPathsAreDerivedNotTrusted checks the layout both applications use to find
// each other's records. The sender lists a directory, so the two have to agree on the
// naming exactly, and a sequence number that did not sort lexicographically would make
// the sender process acknowledgements out of order.
func TestAckPathsAreDerivedNotTrusted(t *testing.T) {
	id := uuid.MustParse("6f9619ff-8b86-d011-b42d-00cf4fc964ff")

	require.Equal(t, "acks/6f9619ff-8b86-d011-b42d-00cf4fc964ff", protocol.AckDir(id))
	require.Equal(t,
		"acks/6f9619ff-8b86-d011-b42d-00cf4fc964ff/00000000000000000417.json",
		protocol.AckPath(id, 417))

	// Zero-padded so that listing the directory yields the records in sequence order.
	require.Less(t, protocol.AckPath(id, 9), protocol.AckPath(id, 10))
	require.Less(t, protocol.AckPath(id, 99999), protocol.AckPath(id, 100000))

	// The temporary name a record is written under must never be mistaken for a
	// record, or a sender would read a half-written file.
	tmp := protocol.AckTempPath(id, 417)
	require.NotEqual(t, protocol.AckPath(id, 417), tmp)
	require.Contains(t, tmp, ".tmp")
}
