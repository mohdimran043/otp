package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"

	"github.com/google/uuid"
)

// TransferAccepted is the sender's answer to a submitted transfer.
//
// It is declared here rather than imported from the sender, because the sender's API types are
// internal to it — and that is the right way round: a caller of an HTTP API works from the JSON it
// receives, not from the server's Go types. Declaring it here means the test is checking the wire
// shape a real client would see.
type TransferAccepted struct {
	TransmissionID uuid.UUID   `json:"transmission_id"`
	FileID         uuid.UUID   `json:"file_id"`
	Filename       string      `json:"filename"`
	SizeBytes      int64       `json:"size_bytes"`
	SHA256         string      `json:"sha256"`
	CallbackURL    string      `json:"callback_url"`
	Status         string      `json:"status"`
	Jobs           []uuid.UUID `json:"jobs"`
}

// postTransfer submits a file and a callback URL to the sender's API.
//
// It builds the request by hand rather than through a generated client, because the point of using it
// in the tests is to exercise the same wire shape a caller would send: a multipart form with a file
// part and a callback_url field, and nothing else required.
func postTransfer(ctx context.Context, url, filename string, body []byte, callbackURL string) (TransferAccepted, error) {
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)

	if callbackURL != "" {
		if err := form.WriteField("callback_url", callbackURL); err != nil {
			return TransferAccepted{}, err
		}
	}
	if err := form.WriteField("filename", filename); err != nil {
		return TransferAccepted{}, err
	}

	part, err := form.CreateFormFile("file", filename)
	if err != nil {
		return TransferAccepted{}, err
	}
	if _, err := part.Write(body); err != nil {
		return TransferAccepted{}, err
	}
	if err := form.Close(); err != nil {
		return TransferAccepted{}, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return TransferAccepted{}, err
	}
	request.Header.Set("Content-Type", form.FormDataContentType())

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return TransferAccepted{}, err
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return TransferAccepted{}, err
	}
	if response.StatusCode != http.StatusAccepted {
		return TransferAccepted{}, fmt.Errorf("e2e: the sender answered %d: %s",
			response.StatusCode, raw)
	}

	var accepted TransferAccepted
	if err := json.Unmarshal(raw, &accepted); err != nil {
		return TransferAccepted{}, err
	}
	return accepted, nil
}

// GetJSON fetches and decodes a JSON endpoint, for tests that assert on the API rather than the
// database.
func GetJSON(ctx context.Context, url string, into any) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return response.StatusCode, err
	}
	if into != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, into); err != nil {
			return response.StatusCode, fmt.Errorf("e2e: %s: %w (%s)", url, err, raw)
		}
	}
	return response.StatusCode, nil
}
