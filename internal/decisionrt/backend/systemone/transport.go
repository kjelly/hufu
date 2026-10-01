package systemone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

const maximumResponseBytes = 64 << 10

func (b *backend) post(ctx context.Context, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, backendFailure(fmt.Errorf("building systemone request: %w", transportCause(err)))
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if b.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+b.apiKey)
	}

	response, err := b.client.Do(request)
	if err != nil {
		return nil, backendFailure(fmt.Errorf("sending systemone request: %w", transportCause(err)))
	}
	defer func() { _ = response.Body.Close() }()
	if err := classifyStatus(response.StatusCode); err != nil {
		return nil, err
	}

	data, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	if err != nil {
		return nil, backendFailure(fmt.Errorf("reading systemone response: %w", transportCause(err)))
	}
	if len(data) > maximumResponseBytes {
		return nil, invalidOutput("response exceeds 64 KiB")
	}
	return data, nil
}

// classifyStatus maps a non-2xx status to an error kind by code alone; the
// provider's error text is never read.
func classifyStatus(status int) error {
	if status >= 200 && status <= 299 {
		return nil
	}
	cause := fmt.Errorf("systemone returned HTTP status %d", status)
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusMethodNotAllowed, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return backendUnavailable(cause)
	default:
		return backendFailure(cause)
	}
}

// transportCause drops the request URL that *url.Error adds to its message.
func transportCause(err error) error {
	if urlError, ok := errors.AsType[*url.Error](err); ok {
		return urlError.Err
	}
	return err
}
