package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opticaltransport/otp/shared/protocol"

	"github.com/opticaltransport/otp/receiver/internal/config"
)

// Delivery errors.
var (
	// ErrCallbackRefused means the receiver would not make the request. It is a refusal rather than
	// a failure: nothing was attempted.
	ErrCallbackRefused = errors.New("pipeline: callback URL refused")

	// ErrCallbackRejected means the endpoint answered with a status that is not success.
	ErrCallbackRejected = errors.New("pipeline: callback endpoint rejected the delivery")
)

// Delivery is a merged file on its way to a callback URL.
type Delivery struct {
	TransmissionID uuid.UUID
	Filename       string

	// SHA256 is the hash the receiver verified the file against, sent as a header so the endpoint
	// can check the body it received independently rather than taking the receiver's word for it.
	SHA256 string

	Body []byte
}

// Deliverer posts merged files to callback URLs.
//
// It is a type rather than a function because of what has to be decided before any request is made.
// The URL crossed the optical channel, so the receiver is being told by the far side of an air gap
// to make an outbound connection — and a service that does that without restraint is a request
// forgery proxy: whoever controls the sender chooses what the receiver connects to, including
// addresses inside the receiver's network that nothing outside it can otherwise reach.
//
// So delivery is gated twice. The URL's shape is checked by the protocol package, and its host is
// checked against an operator-configured allowlist here. A deployment that genuinely trusts its
// sender can turn the allowlist off, which is a decision an operator makes explicitly rather than
// one this code makes for them.
type Deliverer struct {
	cfg    config.Callback
	log    *zap.Logger
	client *http.Client
}

// NewDeliverer returns a deliverer.
func NewDeliverer(cfg config.Callback, log *zap.Logger) *Deliverer {
	return &Deliverer{
		cfg: cfg,
		log: log.Named("callback"),
		client: &http.Client{
			Timeout: cfg.Timeout,
			// Redirects are not followed. A redirect is the far side choosing a second destination
			// after the first has already been checked, which would make the allowlist decorative.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Allowed reports whether a URL may be delivered to, and why not when it may not.
func (d *Deliverer) Allowed(raw string) error {
	if err := protocol.CheckCallbackURL(raw); err != nil {
		return fmt.Errorf("%w: %s", ErrCallbackRefused, err)
	}
	if raw == "" {
		return fmt.Errorf("%w: no URL", ErrCallbackRefused)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrCallbackRefused, err)
	}
	if d.cfg.AllowAnyHost {
		return nil
	}

	host := u.Hostname()
	for _, allowed := range d.cfg.AllowedHosts {
		if strings.EqualFold(host, allowed) {
			return nil
		}
		// A leading dot allows a domain and everything under it, which is how an operator writes
		// "anywhere in our own domain" without listing every host.
		if strings.HasPrefix(allowed, ".") && strings.HasSuffix(strings.ToLower(host), strings.ToLower(allowed)) {
			return nil
		}
	}
	return fmt.Errorf("%w: %q is not in the allowed-hosts list", ErrCallbackRefused, host)
}

// Post delivers a merged file and returns the status code the endpoint answered with.
//
// The file is sent as the request body rather than described in a JSON notification, because that is
// what was asked of the receiver: somebody handed the sender a file and a URL, and the file is what
// should arrive at the URL. Everything else the endpoint needs to identify and check it travels in
// headers, so the body is exactly the bytes that were transferred and nothing else.
func (d *Deliverer) Post(ctx context.Context, raw string, delivery Delivery) (int, error) {
	if err := d.Allowed(raw); err != nil {
		return 0, err
	}
	if int64(len(delivery.Body)) > d.cfg.MaxBodyBytes {
		return 0, fmt.Errorf("%w: %d bytes exceeds the %d permitted",
			ErrCallbackRefused, len(delivery.Body), d.cfg.MaxBodyBytes)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, raw, bytes.NewReader(delivery.Body))
	if err != nil {
		return 0, fmt.Errorf("%w: %s", ErrCallbackRefused, err)
	}

	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", delivery.Filename))
	request.Header.Set("X-OTP-Transmission-Id", delivery.TransmissionID.String())
	request.Header.Set("X-OTP-Filename", delivery.Filename)
	// The hash the receiver verified against, so the endpoint can check the body itself. An
	// endpoint that trusts the header alone has learned nothing; one that recomputes it has
	// end-to-end assurance from the sender's disk to its own.
	request.Header.Set("X-OTP-SHA256", delivery.SHA256)
	request.Header.Set("X-OTP-Verified", "true")
	request.ContentLength = int64(len(delivery.Body))

	response, err := d.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()

	// The response body is read and discarded up to a bound, so the connection can be reused and a
	// misbehaving endpoint cannot stream unbounded data back at the receiver.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return response.StatusCode, fmt.Errorf("%w: status %d", ErrCallbackRejected, response.StatusCode)
	}
	return response.StatusCode, nil
}

// IsPrivateHost reports whether a host resolves only to addresses inside private or loopback
// ranges.
//
// It is offered for an operator's benefit rather than used as a gate, and the distinction matters.
// Resolving a name and then connecting is two separate lookups, so a name that answered publicly
// during the check can answer privately during the request — the rebinding problem — which means a
// resolution-time check cannot be a security boundary. The allowlist can, because it never resolves
// anything.
func IsPrivateHost(host string) bool {
	addresses, err := net.LookupIP(host)
	if err != nil || len(addresses) == 0 {
		return false
	}
	for _, ip := range addresses {
		if !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() {
			return false
		}
	}
	return true
}
