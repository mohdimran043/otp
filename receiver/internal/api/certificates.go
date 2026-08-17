package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/receiver/internal/store"
)

// Managing the two certificates this side holds.
//
// One is generated here and never leaves; the other arrives from the machine across the gap. The whole of
// the trust in this scheme is an operator installing the second, which is why the fingerprint is shown
// everywhere it can be: it is the thing they compare between two screens to know they installed the right
// one, and it is the only check standing between a working pair and a pair that will never open a frame.

const maxCertificateBytes = 16 << 10

// defaultCertificateName names a generated certificate, so an operator looking at two of them can tell
// which machine each came from without reading a fingerprint.
const defaultCertificateName = "otp-receiver"

// certificateStatus is what both sides can do right now.
type certificateStatus struct {
	// Local is this machine's own certificate, absent until one is generated.
	Local *store.Certificate `json:"local,omitempty"`

	// Peer is the other machine's, absent until one is installed.
	Peer *store.Certificate `json:"peer,omitempty"`

	// Ready is whether certificate encryption can actually be used, which needs both halves. Reported as
	// its own field rather than left for a caller to infer from two absences, because it is the question
	// every caller is really asking.
	Ready bool `json:"ready"`

	// Note says what is missing, in the operator's terms.
	Note string `json:"note"`
}

// getCertificates reports what is installed.
func (s *Server) getCertificates(w http.ResponseWriter, r *http.Request) {
	s.respond(w, http.StatusOK, s.certificateStatus(r))
}

// generateCertificate creates a new keypair for this machine, replacing any existing one.
//
// Replacing is destructive in a way worth stating: the other side is holding the *old* public certificate,
// so from the moment this returns until the new one is installed over there, nothing can be sealed or
// opened. That is why it is a deliberate POST rather than something that happens automatically when a
// certificate is found missing.
func (s *Server) generateCertificate(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name string `json:"name,omitempty"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCertificateBytes))
	if err == nil && len(bytes.TrimSpace(body)) > 0 {
		_ = json.Unmarshal(body, &request)
	}
	if request.Name == "" {
		request.Name = defaultCertificateName
	}

	identity, err := protocol.NewIdentity(request.Name)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not generate a certificate", err)
		return
	}
	if err := s.store.Certificates.SaveLocal(r.Context(), identity); err != nil {
		s.fail(w, http.StatusInternalServerError, "could not store the certificate", err)
		return
	}

	s.log.Info("generated a certificate",
		zap.String("subject", identity.Subject),
		zap.String("fingerprint", identity.Fingerprint))

	s.respond(w, http.StatusOK, s.certificateStatus(r))
}

// installPeerCertificate stores the other machine's public certificate.
func (s *Server) installPeerCertificate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCertificateBytes+1))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "could not read the request", err)
		return
	}
	if len(body) > maxCertificateBytes {
		s.fail(w, http.StatusRequestEntityTooLarge, "that is too large to be a certificate", nil)
		return
	}

	// Either a bare PEM or a JSON object carrying one, because an operator pasting from a file has the
	// first and a page posting has the second, and refusing either would be a pointless distinction.
	certPEM := bytes.TrimSpace(body)
	if bytes.HasPrefix(certPEM, []byte("{")) {
		var request struct {
			CertificatePEM string `json:"certificate_pem"`
		}
		if err := json.Unmarshal(certPEM, &request); err != nil {
			s.fail(w, http.StatusBadRequest, "the request is not a certificate", err)
			return
		}
		certPEM = []byte(request.CertificatePEM)
	}

	installed, err := s.store.Certificates.SavePeer(r.Context(), certPEM)
	if err != nil {
		if errors.Is(err, protocol.ErrNotACertificate) || errors.Is(err, protocol.ErrWrongKeyType) {
			s.fail(w, http.StatusBadRequest, err.Error(), err)
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not store the certificate", err)
		return
	}

	// Loud, because installing the wrong certificate is the failure this scheme has, and a log line with
	// the fingerprint is what lets it be traced afterwards.
	s.log.Info("installed the peer certificate",
		zap.String("subject", installed.Subject),
		zap.String("fingerprint", installed.Fingerprint))

	s.respond(w, http.StatusOK, s.certificateStatus(r))
}

// deletePeerCertificate removes the installed peer certificate.
func (s *Server) deletePeerCertificate(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Certificates.Delete(r.Context(), store.RolePeer); err != nil {
		s.fail(w, http.StatusInternalServerError, "could not remove the certificate", err)
		return
	}
	s.respond(w, http.StatusOK, s.certificateStatus(r))
}

// certificateStatus reads both roles and says what can be done.
func (s *Server) certificateStatus(r *http.Request) certificateStatus {
	var out certificateStatus

	if local, err := s.store.Certificates.Get(r.Context(), store.RoleLocal); err == nil {
		out.Local = &local
	}
	if peer, err := s.store.Certificates.Get(r.Context(), store.RolePeer); err == nil {
		out.Peer = &peer
	}

	switch {
	case out.Local == nil && out.Peer == nil:
		out.Note = "Generate this side's certificate, then give its PEM to the other side and install " +
			"theirs here. Neither half is secret except the private key, which never leaves this machine."
	case out.Local == nil:
		out.Note = "Generate this side's certificate. The other side's is installed and waiting."
	case out.Peer == nil:
		out.Note = "Install the other side's certificate. Copy the PEM shown here to them as well — " +
			"each side needs the other's, and a pair only works when both are in place."
	case out.Local.Expired() || out.Peer.Expired():
		out.Note = "A certificate has expired. Generate a new one here and install it there."
	default:
		out.Ready = true
		out.Note = "Both certificates are in place. Compare the fingerprints against the other side's " +
			"screen: they are what tells you the right certificate was installed."
	}
	return out
}
