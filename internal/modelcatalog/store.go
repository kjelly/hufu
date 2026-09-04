package modelcatalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultUpdateTimeout = 15 * time.Second
	defaultMaxResponse   = 16 << 20
)

// ErrNoNet reports that an explicit models update was refused before HTTP.
var ErrNoNet = errors.New("models catalog update blocked by no-net policy")

var errCatalogNoNet = ErrNoNet

// StoreOptions provides the seams used by offline/runtime tests and command
// integration. Update is the only Store method that performs HTTP.
type StoreOptions struct {
	Clock            func() time.Time
	Client           *http.Client
	SourceURL        string
	NoNet            bool
	MaxResponseBytes int64
	Rename           func(oldPath, newPath string) error
}

// Store loads cache data and owns explicit models.dev updates.
type Store struct {
	Path            string
	clock           func() time.Time
	client          *http.Client
	sourceURL       string
	noNet           bool
	maxResponseSize int64
	rename          func(string, string) error
}

func NewStore(path string, options ...StoreOptions) *Store {
	var option StoreOptions
	if len(options) > 0 {
		option = options[0]
	}
	if path == "" {
		path = DefaultPath()
	}
	clock := option.Clock
	if clock == nil {
		clock = time.Now
	}
	client := option.Client
	if client == nil {
		client = &http.Client{}
	}
	sourceURL := option.SourceURL
	if sourceURL == "" {
		sourceURL = DefaultSourceURL
	}
	maxResponse := option.MaxResponseBytes
	if maxResponse <= 0 {
		maxResponse = defaultMaxResponse
	}
	rename := option.Rename
	if rename == nil {
		rename = os.Rename
	}
	return &Store{Path: path, clock: clock, client: client, sourceURL: sourceURL, noNet: option.NoNet, maxResponseSize: maxResponse, rename: rename}
}

func NewDefaultStore(options ...StoreOptions) *Store { return NewStore(DefaultPath(), options...) }

// Load returns cache data, falling back to the embedded snapshot for missing
// or invalid cache files. Runtime callers never write cache data.
func (s *Store) Load() (Catalog, error) {
	catalog, _, err := s.LoadWithOrigin()
	return catalog, err
}

// LoadWithOrigin reports whether the validated snapshot came from cache or
// the embedded fallback. Cache errors are intentionally not returned when the
// embedded snapshot is valid.
func (s *Store) LoadWithOrigin() (Catalog, string, error) {
	if s != nil && strings.TrimSpace(s.Path) != "" {
		if data, err := os.ReadFile(s.Path); err == nil {
			if catalog, parseErr := parseCatalogFile(data); parseErr == nil {
				return catalog, OriginCache, nil
			}
		}
	}
	catalog, err := parseCatalogFile(embeddedSnapshot)
	if err != nil {
		return Catalog{}, "", fmt.Errorf("embedded catalog is invalid: %w", err)
	}
	return catalog, OriginEmbedded, nil
}

// Update downloads, validates, normalizes, and atomically replaces the local
// cache. All failures happen before rename so the old cache remains intact.
func (s *Store) Update(ctx context.Context) (Catalog, error) {
	if s == nil {
		return Catalog{}, errors.New("catalog store unavailable")
	}
	if s.noNet {
		return Catalog{}, errCatalogNoNet
	}
	parsedURL, err := url.Parse(strings.TrimSpace(s.sourceURL))
	if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" || parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return Catalog{}, errors.New("catalog source must be an HTTPS URL without credentials or query parameters")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	requestContext, cancel := context.WithTimeout(ctx, defaultUpdateTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return Catalog{}, fmt.Errorf("create catalog request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	client := *s.client
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return errors.New("catalog redirect refused")
	}
	response, err := client.Do(request)
	if err != nil {
		return Catalog{}, fmt.Errorf("download catalog: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return Catalog{}, fmt.Errorf("catalog returned status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, s.maxResponseSize+1))
	if err != nil {
		return Catalog{}, fmt.Errorf("read catalog: %w", err)
	}
	if int64(len(data)) > s.maxResponseSize {
		return Catalog{}, errors.New("catalog response exceeds size limit")
	}
	catalog, err := ParseJSON(data)
	if err != nil {
		return Catalog{}, fmt.Errorf("normalize catalog: %w", err)
	}
	encoded, err := catalog.marshal()
	if err != nil {
		return Catalog{}, fmt.Errorf("encode normalized catalog: %w", err)
	}
	if err := s.atomicReplace(encoded); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

func (s *Store) atomicReplace(data []byte) error {
	directory := filepath.Dir(s.Path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create catalog cache directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".models-*.tmp")
	if err != nil {
		return fmt.Errorf("create catalog cache temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	keepTemporary := true
	defer func() {
		if keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set catalog cache permissions: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write catalog cache: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync catalog cache: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close catalog cache: %w", err)
	}
	if err := s.rename(temporaryPath, s.Path); err != nil {
		return fmt.Errorf("replace catalog cache: %w", err)
	}
	keepTemporary = false
	return nil
}
