// Package providerproxy owns the hard-abort boundary for provider HTTP calls.
//
// The proxy is deliberately a separate Hufu process.  Fantasy owns the HTTP
// client and does not expose an abort/close operation; killing this process
// closes the provider connection and makes a blocked response return to the
// Hufu caller.  The control handshake is versioned and carries credentials
// only over an anonymous pipe; it is never logged or persisted.
package providerproxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	ProtocolVersion = "hufu-provider-proxy/v1"
	ChildArg        = "--hufu-provider-proxy-child"
	capabilityPath  = "/.hufu-provider/"
)

type Config struct {
	UpstreamURL string `json:"upstream_url"`
	APIKey      string `json:"api_key,omitempty"`
}

type readyMessage struct {
	Version   string `json:"version"`
	URL       string `json:"url"`
	Error     string `json:"error,omitempty"`
	ErrorKind string `json:"error_kind,omitempty"`
}

type controlMessage struct {
	Version string `json:"version"`
	Config  Config `json:"config"`
}

// Proxy is the parent-side owner of one provider proxy process. Close is
// idempotent and does not return until the process has been reaped.
type Proxy struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	url    string

	closeOnce sync.Once
	closeErr  error
}

// ErrListenerUnavailable reports that the subprocess boundary could not
// acquire its loopback listener. Callers may select an equivalent owned
// boundary backend for this error, but must not fall back to a direct client.
var ErrListenerUnavailable = errors.New("provider proxy listener unavailable")

// Boundary is the invocation-scoped ownership contract for provider HTTP
// calls. A nil HTTPClient means the backend owns the endpoint itself (the
// subprocess proxy); a non-nil client is an in-process connection-owning
// transport. Abort and Stop are synchronous and terminal for the invocation.
type Boundary interface {
	URL() string
	HTTPClient() *http.Client
	Abort() error
	Stop() error
}

func Start(ctx context.Context, executable string, cfg Config) (*Proxy, error) {
	return start(ctx, executable, []string{ChildArg}, nil, cfg)
}

// StartWithCommand is primarily a deterministic contract-test seam. The
// production path always uses ChildArg; callers should use it only when the
// supplied executable is a test harness that dispatches to RunChild.
func StartWithCommand(ctx context.Context, executable string, args []string, env []string, cfg Config) (*Proxy, error) {
	return start(ctx, executable, args, env, cfg)
}

func start(ctx context.Context, executable string, args, env []string, cfg Config) (*Proxy, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if strings.TrimSpace(executable) == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve hufu provider proxy executable: %w", err)
		}
	}
	cmd := exec.Command(executable, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	configureProcessAttributes(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create provider proxy control pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("create provider proxy status pipe: %w", err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start provider proxy: %w", err)
	}
	p := &Proxy{cmd: cmd, stdin: stdin, stdout: stdout}
	control := controlMessage{Version: ProtocolVersion, Config: cfg}
	encoded, err := json.Marshal(control)
	if err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("encode provider proxy control message: %w", err)
	}
	if _, err := stdin.Write(append(encoded, '\n')); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("send provider proxy control message: %w", err)
	}
	_ = stdin.Close()

	readyCh := make(chan struct {
		ready readyMessage
		err   error
	}, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if !scanner.Scan() {
			if scanErr := scanner.Err(); scanErr != nil {
				readyCh <- struct {
					ready readyMessage
					err   error
				}{err: scanErr}
				return
			}
			readyCh <- struct {
				ready readyMessage
				err   error
			}{err: errors.New("provider proxy exited before readiness")}
			return
		}
		var ready readyMessage
		if err := json.Unmarshal(scanner.Bytes(), &ready); err != nil {
			readyCh <- struct {
				ready readyMessage
				err   error
			}{err: fmt.Errorf("decode provider proxy readiness: %w", err)}
			return
		}
		readyCh <- struct {
			ready readyMessage
			err   error
		}{ready: ready}
	}()
	select {
	case <-ctx.Done():
		_ = p.Close()
		return nil, fmt.Errorf("provider proxy startup cancelled: %w", ctx.Err())
	case result := <-readyCh:
		if result.err != nil {
			_ = p.Close()
			return nil, result.err
		}
		if result.ready.Error != "" {
			_ = p.Close()
			if result.ready.ErrorKind == "listener_unavailable" {
				return nil, fmt.Errorf("provider proxy startup: %s: %w", result.ready.Error, ErrListenerUnavailable)
			}
			return nil, fmt.Errorf("provider proxy startup: %s", result.ready.Error)
		}
		if result.ready.Version != ProtocolVersion || !validProxyURL(result.ready.URL) {
			_ = p.Close()
			return nil, errors.New("provider proxy returned invalid readiness protocol")
		}
		p.url = result.ready.URL
		return p, nil
	}
}

func validateConfig(cfg Config) error {
	u, err := url.Parse(strings.TrimSpace(cfg.UpstreamURL))
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return errors.New("provider proxy requires an http(s) upstream URL")
	}
	return nil
}

func validProxyURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "http" && u.Host != ""
}

func (p *Proxy) URL() string {
	if p == nil {
		return ""
	}
	return p.url
}

func (p *Proxy) HTTPClient() *http.Client { return nil }

func (p *Proxy) Abort() error { return p.Close() }

func (p *Proxy) Stop() error { return p.Close() }

func (p *Proxy) Close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		if p.stdin != nil {
			_ = p.stdin.Close()
		}
		if p.stdout != nil {
			_ = p.stdout.Close()
		}
		if p.cmd == nil || p.cmd.Process == nil {
			return
		}
		// Terminate the complete process group where the platform supports it.
		// The helper synchronously waits below so the proxy never reports Close
		// before its child has been reaped.
		terminateProcess(p.cmd)
		waitErr := p.cmd.Wait()
		if waitErr != nil {
			var exitErr *exec.ExitError
			if !errors.As(waitErr, &exitErr) {
				p.closeErr = fmt.Errorf("reap provider proxy: %w", waitErr)
			}
		}
	})
	return p.closeErr
}

// RunChild is called only by the Hufu executable's hidden child mode.
func RunChild(in io.Reader, out io.Writer) int {
	decoder := json.NewDecoder(in)
	var control controlMessage
	if err := decoder.Decode(&control); err != nil || control.Version != ProtocolVersion || validateConfig(control.Config) != nil {
		return 2
	}
	u, _ := url.Parse(control.Config.UpstreamURL)
	token, err := newCapabilityToken()
	if err != nil {
		message := readyMessage{Version: ProtocolVersion, Error: "generate provider proxy capability: " + err.Error()}
		if data, marshalErr := json.Marshal(message); marshalErr == nil {
			_, _ = out.Write(append(data, '\n'))
		}
		return 3
	}
	privatePath := capabilityPath + token
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		message := readyMessage{Version: ProtocolVersion, Error: "listen provider proxy: " + err.Error()}
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			message.ErrorKind = "listener_unavailable"
		}
		if data, marshalErr := json.Marshal(message); marshalErr == nil {
			_, _ = out.Write(append(data, '\n'))
		}
		return 3
	}
	defer func() { _ = listener.Close() }()
	// Use Rewrite as the sole routing mechanism. NewSingleHostReverseProxy
	// installs a Director; combining that implicit Director with Rewrite makes
	// Go 1.26 reject the request before it reaches the upstream.
	proxy := &httputil.ReverseProxy{}
	proxy.Rewrite = func(req *httputil.ProxyRequest) {
		stripCapabilityPath(req.Out, privatePath)
		req.SetURL(u)
		req.Out.Header.Set("X-Hufu-Provider-Proxy-Version", ProtocolVersion)
		if req.Out.Header.Get("Authorization") == "" && control.Config.APIKey != "" {
			req.Out.Header.Set("Authorization", "Bearer "+control.Config.APIKey)
		}
	}
	proxy.Transport = &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext}
	server := &http.Server{Handler: requireCapability(privatePath, proxy), ReadHeaderTimeout: 10 * time.Second}
	ready := readyMessage{Version: ProtocolVersion, URL: "http://" + listener.Addr().String() + privatePath}
	data, _ := json.Marshal(ready)
	if _, err := out.Write(append(data, '\n')); err != nil {
		return 4
	}
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return 5
	}
	return 0
}

func newCapabilityToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(token[:]), nil
}

func requireCapability(privatePath string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hasCapabilityPath(r.URL, privatePath) {
			http.Error(w, "unauthorized provider proxy request", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hasCapabilityPath(requestURL *url.URL, privatePath string) bool {
	if requestURL == nil {
		return false
	}
	escapedPath := requestURL.EscapedPath()
	return escapedPath == privatePath || strings.HasPrefix(escapedPath, privatePath+"/")
}

func stripCapabilityPath(request *http.Request, privatePath string) {
	request.URL.Path = strings.TrimPrefix(request.URL.Path, privatePath)
	if request.URL.Path == "" {
		request.URL.Path = "/"
	}
	if request.URL.RawPath != "" {
		request.URL.RawPath = strings.TrimPrefix(request.URL.RawPath, privatePath)
		if request.URL.RawPath == "" {
			request.URL.RawPath = "/"
		}
	}
}
