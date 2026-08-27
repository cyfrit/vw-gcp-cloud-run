package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	healthPath        = "/_vwshim/healthz"
	readyPath         = "/_vwshim/readyz"
	gcsRequestTimeout = 10 * time.Second
)

var (
	errNotFound     = errors.New("object not found")
	errPrecondition = errors.New("object precondition failed")
)

type config struct {
	listenAddr       string
	upstreamURL      *url.URL
	databasePath     string
	replicaURL       string
	litestreamBin    string
	litestreamConfig string
	litestreamSocket string
	vaultwardenBin   string
	bucket           string
	leaseObject      string
	takeoverObject   string
	revision         string
	instanceID       string
	leaseTTL         time.Duration
	renewInterval    time.Duration
	pollInterval     time.Duration
	syncTimeout      time.Duration
	startupTimeout   time.Duration
}

func loadConfig() (config, error) {
	port := envOr("PORT", "8080")
	upstream, err := url.Parse("http://" + envOr("VAULTWARDEN_ADDR", "127.0.0.1:8081"))
	if err != nil {
		return config{}, fmt.Errorf("parse VAULTWARDEN_ADDR: %w", err)
	}

	leaseTTL, err := envDuration("LEASE_TTL", 30*time.Second)
	if err != nil {
		return config{}, err
	}
	renewInterval, err := envDuration("LEASE_RENEW_INTERVAL", 5*time.Second)
	if err != nil {
		return config{}, err
	}
	pollInterval, err := envDuration("LEASE_POLL_INTERVAL", 2*time.Second)
	if err != nil {
		return config{}, err
	}
	syncTimeout, err := envDuration("SYNC_TIMEOUT", 5*time.Second)
	if err != nil {
		return config{}, err
	}
	startupTimeout, err := envDuration("STARTUP_TIMEOUT", 2*time.Minute)
	if err != nil {
		return config{}, err
	}
	if leaseTTL < 3*gcsRequestTimeout {
		return config{}, fmt.Errorf("LEASE_TTL must be at least %s", 3*gcsRequestTimeout)
	}
	if renewInterval >= leaseTTL/4 {
		return config{}, fmt.Errorf("LEASE_RENEW_INTERVAL must be less than one quarter of LEASE_TTL")
	}

	bucket := os.Getenv("GCS_BUCKET")
	if bucket == "" {
		return config{}, fmt.Errorf("GCS_BUCKET is required")
	}
	replicaURL := os.Getenv("LITESTREAM_REPLICA_URL")
	if replicaURL == "" {
		return config{}, fmt.Errorf("LITESTREAM_REPLICA_URL is required")
	}
	prefix := strings.Trim(envOr("CONTROL_PREFIX", "vaultwarden/control"), "/")
	instanceID, err := randomID()
	if err != nil {
		return config{}, fmt.Errorf("create instance id: %w", err)
	}

	return config{
		listenAddr:       ":" + port,
		upstreamURL:      upstream,
		databasePath:     envOr("DATABASE_PATH", "/var/lib/vaultwarden/db.sqlite3"),
		replicaURL:       replicaURL,
		litestreamBin:    envOr("LITESTREAM_BIN", "/usr/local/bin/litestream"),
		litestreamConfig: envOr("LITESTREAM_CONFIG", "/etc/litestream.yml"),
		litestreamSocket: envOr("LITESTREAM_SOCKET", "/var/run/litestream.sock"),
		vaultwardenBin:   envOr("VAULTWARDEN_BIN", "/start.sh"),
		bucket:           bucket,
		leaseObject:      prefix + "/lease.json",
		takeoverObject:   prefix + "/takeover.json",
		revision:         envOr("K_REVISION", "local"),
		instanceID:       instanceID,
		leaseTTL:         leaseTTL,
		renewInterval:    renewInterval,
		pollInterval:     pollInterval,
		syncTimeout:      syncTimeout,
		startupTimeout:   startupTimeout,
	}, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return d, nil
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type storedObject struct {
	data       []byte
	generation int64
	updated    time.Time
	serverTime time.Time
}

type objectStore interface {
	get(context.Context, string, string) (storedObject, error)
	put(context.Context, string, string, []byte, int64) (storedObject, error)
	delete(context.Context, string, string, int64) error
}

type metadataTokenSource struct {
	client *http.Client
	mu     sync.Mutex
	token  string
	expiry time.Time
}

func (s *metadataTokenSource) get(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Until(s.expiry) > time.Minute {
		return s.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("get metadata token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get metadata token: status %s", resp.Status)
	}
	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode metadata token: %w", err)
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("metadata server returned an empty token")
	}
	s.token = result.AccessToken
	s.expiry = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	return s.token, nil
}

type gcsStore struct {
	client *http.Client
	tokens *metadataTokenSource
}

func newGCSStore() *gcsStore {
	client := &http.Client{Timeout: gcsRequestTimeout}
	return &gcsStore{client: client, tokens: &metadataTokenSource{client: client}}
}

func (s *gcsStore) authorize(ctx context.Context, req *http.Request) error {
	token, err := s.tokens.get(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func (s *gcsStore) get(ctx context.Context, bucket, name string) (storedObject, error) {
	endpoint := fmt.Sprintf("https://storage.googleapis.com/download/storage/v1/b/%s/o/%s?alt=media",
		url.PathEscape(bucket), url.PathEscape(name))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return storedObject{}, err
	}
	req.Header.Set("Cache-Control", "no-cache")
	if err := s.authorize(ctx, req); err != nil {
		return storedObject{}, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return storedObject{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return storedObject{}, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return storedObject{}, fmt.Errorf("get gs://%s/%s: status %s", bucket, name, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return storedObject{}, err
	}
	generation, err := strconv.ParseInt(resp.Header.Get("X-Goog-Generation"), 10, 64)
	if err != nil {
		return storedObject{}, fmt.Errorf("parse GCS generation: %w", err)
	}
	updated, err := http.ParseTime(resp.Header.Get("Last-Modified"))
	if err != nil {
		return storedObject{}, fmt.Errorf("parse GCS Last-Modified: %w", err)
	}
	serverTime, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return storedObject{}, fmt.Errorf("parse GCS Date: %w", err)
	}
	return storedObject{data: data, generation: generation, updated: updated, serverTime: serverTime}, nil
}

func (s *gcsStore) put(ctx context.Context, bucket, name string, data []byte, generation int64) (storedObject, error) {
	query := url.Values{}
	query.Set("uploadType", "media")
	query.Set("name", name)
	query.Set("ifGenerationMatch", strconv.FormatInt(generation, 10))
	endpoint := fmt.Sprintf("https://storage.googleapis.com/upload/storage/v1/b/%s/o?%s",
		url.PathEscape(bucket), query.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return storedObject{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := s.authorize(ctx, req); err != nil {
		return storedObject{}, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return storedObject{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPreconditionFailed {
		return storedObject{}, errPrecondition
	}
	if resp.StatusCode != http.StatusOK {
		return storedObject{}, fmt.Errorf("put gs://%s/%s: status %s", bucket, name, resp.Status)
	}
	var result struct {
		Generation string `json:"generation"`
		Updated    string `json:"updated"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return storedObject{}, fmt.Errorf("decode GCS object metadata: %w", err)
	}
	newGeneration, err := strconv.ParseInt(result.Generation, 10, 64)
	if err != nil {
		return storedObject{}, fmt.Errorf("parse GCS generation: %w", err)
	}
	updated, err := time.Parse(time.RFC3339Nano, result.Updated)
	if err != nil {
		return storedObject{}, fmt.Errorf("parse GCS updated time: %w", err)
	}
	serverTime, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return storedObject{}, fmt.Errorf("parse GCS Date: %w", err)
	}
	return storedObject{data: data, generation: newGeneration, updated: updated, serverTime: serverTime}, nil
}

func (s *gcsStore) delete(ctx context.Context, bucket, name string, generation int64) error {
	query := url.Values{}
	query.Set("ifGenerationMatch", strconv.FormatInt(generation, 10))
	endpoint := fmt.Sprintf("https://storage.googleapis.com/storage/v1/b/%s/o/%s?%s",
		url.PathEscape(bucket), url.PathEscape(name), query.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	if err := s.authorize(ctx, req); err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound:
		return nil
	case http.StatusPreconditionFailed:
		return errPrecondition
	default:
		return fmt.Errorf("delete gs://%s/%s: status %s", bucket, name, resp.Status)
	}
}

type leasePayload struct {
	Version  int    `json:"version"`
	Owner    string `json:"owner"`
	Revision string `json:"revision"`
}

type takeoverPayload struct {
	Version   int    `json:"version"`
	Requester string `json:"requester"`
	Revision  string `json:"revision"`
}

type leaseEvent struct {
	err        error
	stillOwner bool
}

type leaseManager struct {
	store          objectStore
	bucket         string
	leaseObject    string
	takeoverObject string
	owner          string
	revision       string
	ttl            time.Duration
	renewInterval  time.Duration
	pollInterval   time.Duration

	mu          sync.RWMutex
	opMu        sync.Mutex
	generation  int64
	lastRenewal time.Time
}

func (m *leaseManager) payload() []byte {
	data, _ := json.Marshal(leasePayload{Version: 1, Owner: m.owner, Revision: m.revision})
	return data
}

func (m *leaseManager) valid() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.generation != 0 && time.Since(m.lastRenewal) < m.localValidityWindow()
}

// localValidityWindow is deliberately shorter than the remote TTL. A GCS
// response can arrive well after the object update occurred, so using the full
// TTL from receipt time could let an old owner serve after remote expiry.
func (m *leaseManager) localValidityWindow() time.Duration {
	return m.ttl / 2
}

func (m *leaseManager) setLease(generation int64) {
	m.mu.Lock()
	m.generation = generation
	m.lastRenewal = time.Now()
	m.mu.Unlock()
}

func (m *leaseManager) clearLease() {
	m.mu.Lock()
	m.generation = 0
	m.lastRenewal = time.Time{}
	m.mu.Unlock()
}

// ensureValid revalidates an existing lease with a generation-checked write.
// Cloud Run can suspend CPU between requests, so the local validity window may
// expire even though this process still owns the remote object.
func (m *leaseManager) ensureValid(ctx context.Context) error {
	if m.valid() {
		return nil
	}

	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.valid() {
		return nil
	}

	m.mu.RLock()
	generation := m.generation
	m.mu.RUnlock()
	if generation == 0 {
		return fmt.Errorf("lease has no active generation")
	}

	renewCtx, cancel := context.WithTimeout(ctx, min(m.renewInterval, gcsRequestTimeout))
	defer cancel()
	updated, err := m.store.put(renewCtx, m.bucket, m.leaseObject, m.payload(), generation)
	if errors.Is(err, errPrecondition) {
		m.clearLease()
		return fmt.Errorf("lease ownership lost: %w", err)
	}
	if err != nil {
		return fmt.Errorf("revalidate lease: %w", err)
	}
	m.setLease(updated.generation)
	return nil
}

func (m *leaseManager) acquire(ctx context.Context) error {
	for {
		obj, err := m.store.get(ctx, m.bucket, m.leaseObject)
		switch {
		case errors.Is(err, errNotFound):
			blocked, takeoverErr := m.blockedByTakeover(ctx)
			if takeoverErr != nil {
				slog.Warn("takeover priority check failed", "error", takeoverErr)
				break
			}
			if blocked {
				break
			}
			created, putErr := m.store.put(ctx, m.bucket, m.leaseObject, m.payload(), 0)
			if putErr == nil {
				m.setLease(created.generation)
				return nil
			}
			if !errors.Is(putErr, errPrecondition) {
				slog.Warn("lease create failed", "error", putErr)
			}
		case err != nil:
			slog.Warn("lease read failed", "error", err)
		default:
			var holder leasePayload
			if err := json.Unmarshal(obj.data, &holder); err != nil {
				return fmt.Errorf("decode lease: %w", err)
			}
			if !obj.serverTime.Before(obj.updated.Add(m.ttl)) {
				replaced, putErr := m.store.put(ctx, m.bucket, m.leaseObject, m.payload(), obj.generation)
				if putErr == nil {
					m.setLease(replaced.generation)
					return nil
				}
				if !errors.Is(putErr, errPrecondition) {
					slog.Warn("expired lease replacement failed", "error", putErr)
				}
			} else if revisionNumber(m.revision) > revisionNumber(holder.Revision) {
				if err := m.requestTakeover(ctx); err != nil {
					slog.Warn("revision takeover request failed", "error", err)
				}
			}
		}

		timer := time.NewTimer(m.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *leaseManager) blockedByTakeover(ctx context.Context) (bool, error) {
	obj, err := m.store.get(ctx, m.bucket, m.takeoverObject)
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var request takeoverPayload
	if err := json.Unmarshal(obj.data, &request); err != nil {
		return false, fmt.Errorf("decode takeover request: %w", err)
	}
	if !obj.serverTime.Before(obj.updated.Add(m.ttl)) {
		if err := m.store.delete(ctx, m.bucket, m.takeoverObject, obj.generation); err != nil && !errors.Is(err, errPrecondition) {
			return false, err
		}
		return false, nil
	}
	return revisionNumber(request.Revision) > revisionNumber(m.revision), nil
}

func (m *leaseManager) requestTakeover(ctx context.Context) error {
	request := takeoverPayload{Version: 1, Requester: m.owner, Revision: m.revision}
	data, _ := json.Marshal(request)
	obj, err := m.store.get(ctx, m.bucket, m.takeoverObject)
	if errors.Is(err, errNotFound) {
		_, err = m.store.put(ctx, m.bucket, m.takeoverObject, data, 0)
		if errors.Is(err, errPrecondition) {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	var current takeoverPayload
	if json.Unmarshal(obj.data, &current) == nil && revisionNumber(current.Revision) > revisionNumber(m.revision) {
		return nil
	}
	_, err = m.store.put(ctx, m.bucket, m.takeoverObject, data, obj.generation)
	if errors.Is(err, errPrecondition) {
		return nil
	}
	return err
}

func (m *leaseManager) maintain(ctx context.Context) <-chan leaseEvent {
	ch := make(chan leaseEvent, 1)
	go func() {
		defer close(ch)
		ticker := time.NewTicker(m.renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if event := m.renewAndCheck(ctx); event != nil {
				ch <- *event
				return
			}
		}
	}()
	return ch
}

func (m *leaseManager) renewAndCheck(ctx context.Context) *leaseEvent {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.RLock()
	generation := m.generation
	m.mu.RUnlock()
	if generation == 0 {
		return &leaseEvent{err: fmt.Errorf("lease has no active generation"), stillOwner: false}
	}
	renewCtx, cancelRenew := context.WithTimeout(ctx, min(m.renewInterval, gcsRequestTimeout))
	updated, err := m.store.put(renewCtx, m.bucket, m.leaseObject, m.payload(), generation)
	cancelRenew()
	if err == nil {
		m.setLease(updated.generation)
	} else if errors.Is(err, errPrecondition) {
		m.clearLease()
		return &leaseEvent{err: fmt.Errorf("lease ownership lost: %w", err), stillOwner: false}
	} else if ctx.Err() != nil {
		return nil
	} else {
		slog.Warn("lease renewal failed; requests remain blocked after local expiry", "error", err)
		return nil
	}

	takeoverCtx, cancelTakeover := context.WithTimeout(ctx, min(m.renewInterval, gcsRequestTimeout))
	takeover, err := m.store.get(takeoverCtx, m.bucket, m.takeoverObject)
	cancelTakeover()
	if errors.Is(err, errNotFound) {
		return nil
	}
	if err != nil {
		slog.Warn("takeover request read failed", "error", err)
		return nil
	}
	var request takeoverPayload
	if err := json.Unmarshal(takeover.data, &request); err != nil {
		slog.Warn("invalid takeover request", "error", err)
		return nil
	}
	requestFresh := takeover.serverTime.Before(takeover.updated.Add(m.ttl))
	if requestFresh && revisionNumber(request.Revision) > revisionNumber(m.revision) {
		return &leaseEvent{err: fmt.Errorf("handover requested by revision %s", request.Revision), stillOwner: true}
	}
	if !requestFresh {
		if err := m.store.delete(ctx, m.bucket, m.takeoverObject, takeover.generation); err != nil && !errors.Is(err, errPrecondition) {
			slog.Warn("delete expired takeover request failed", "error", err)
		}
	}
	return nil
}

func (m *leaseManager) clearTakeover(ctx context.Context) {
	obj, err := m.store.get(ctx, m.bucket, m.takeoverObject)
	if err != nil {
		return
	}
	var request takeoverPayload
	if json.Unmarshal(obj.data, &request) == nil && revisionNumber(request.Revision) <= revisionNumber(m.revision) {
		if err := m.store.delete(ctx, m.bucket, m.takeoverObject, obj.generation); err != nil && !errors.Is(err, errPrecondition) {
			slog.Warn("clear takeover request failed", "error", err)
		}
	}
}

func (m *leaseManager) release(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.RLock()
	generation := m.generation
	m.mu.RUnlock()
	if generation == 0 {
		return nil
	}
	if err := m.store.delete(ctx, m.bucket, m.leaseObject, generation); err != nil {
		if errors.Is(err, errPrecondition) {
			m.clearLease()
			return nil
		}
		return err
	}
	m.clearLease()
	return nil
}

var revisionPattern = regexp.MustCompile(`-(\d+)-[^-]+$`)

func revisionNumber(revision string) int64 {
	match := revisionPattern.FindStringSubmatch(revision)
	if len(match) != 2 {
		return -1
	}
	n, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return -1
	}
	return n
}

type managedProcess struct {
	name string
	cmd  *exec.Cmd
	done chan struct{}
	mu   sync.RWMutex
	err  error
}

func startProcess(ctx context.Context, name, command string, args []string, extraEnv []string) (*managedProcess, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = mergedEnv(os.Environ(), extraEnv)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	p := &managedProcess{name: name, cmd: cmd, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}

func mergedEnv(base, overrides []string) []string {
	overridden := make(map[string]struct{}, len(overrides))
	for _, item := range overrides {
		if key, _, ok := strings.Cut(item, "="); ok {
			overridden[key] = struct{}{}
		}
	}
	result := make([]string, 0, len(base)+len(overrides))
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if _, exists := overridden[key]; !exists {
			result = append(result, item)
		}
	}
	return append(result, overrides...)
}

func (p *managedProcess) waitError() error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.err
}

func (p *managedProcess) stop(timeout time.Duration) {
	if p == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(timeout):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func (p *managedProcess) kill() {
	if p == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	<-p.done
}

type syncBarrier struct {
	socket       string
	databasePath string
	timeout      time.Duration
	once         sync.Once
	gate         chan struct{}
}

func (b *syncBarrier) sync(ctx context.Context) error {
	b.once.Do(func() {
		b.gate = make(chan struct{}, 1)
		b.gate <- struct{}{}
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.gate:
	}
	defer func() { b.gate <- struct{}{} }()

	timeout := b.timeout
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	payload, _ := json.Marshal(struct {
		Path    string `json:"path"`
		Wait    bool   `json:"wait"`
		Timeout int    `json:"timeout"`
	}{Path: b.databasePath, Wait: true, Timeout: max(1, int(timeout.Seconds()))})
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", b.socket)
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://litestream/sync", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("litestream sync: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("litestream sync: status %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var result struct {
		TXID           uint64 `json:"txid"`
		ReplicatedTXID uint64 `json:"replicated_txid"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("decode litestream sync: %w", err)
	}
	if result.ReplicatedTXID < result.TXID {
		return fmt.Errorf("litestream sync incomplete: local txid=%d replica txid=%d", result.TXID, result.ReplicatedTXID)
	}
	return nil
}

type proxyState struct {
	ready atomic.Bool
	lease *leaseManager
}

func newProxy(state *proxyState, target *url.URL, barrier *syncBarrier) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		slog.Error("upstream request failed", "error", err)
		http.Error(w, "service temporarily unavailable", http.StatusServiceUnavailable)
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		if isMutation(resp.Request.Method) {
			if err := state.lease.ensureValid(resp.Request.Context()); err != nil {
				return err
			}
			if err := barrier.sync(resp.Request.Context()); err != nil {
				return err
			}
			if err := state.lease.ensureValid(resp.Request.Context()); err != nil {
				return err
			}
		}
		return nil
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case healthPath:
			w.WriteHeader(http.StatusNoContent)
			return
		case readyPath:
			if state.ready.Load() && state.lease.ensureValid(r.Context()) == nil {
				w.WriteHeader(http.StatusNoContent)
			} else {
				http.Error(w, "not ready", http.StatusServiceUnavailable)
			}
			return
		}
		if !state.ready.Load() || state.lease.ensureValid(r.Context()) != nil {
			http.Error(w, "service temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

func isMutation(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

type application struct {
	cfg         config
	lease       *leaseManager
	state       *proxyState
	barrier     *syncBarrier
	server      *http.Server
	litestream  *managedProcess
	vaultwarden *managedProcess
}

func newApplication(cfg config, store objectStore) *application {
	lease := &leaseManager{
		store: store, bucket: cfg.bucket, leaseObject: cfg.leaseObject, takeoverObject: cfg.takeoverObject,
		owner: cfg.instanceID, revision: cfg.revision, ttl: cfg.leaseTTL,
		renewInterval: cfg.renewInterval, pollInterval: cfg.pollInterval,
	}
	state := &proxyState{lease: lease}
	barrier := &syncBarrier{socket: cfg.litestreamSocket, databasePath: cfg.databasePath, timeout: cfg.syncTimeout}
	server := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           newProxy(state, cfg.upstreamURL, barrier),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return &application{cfg: cfg, lease: lease, state: state, barrier: barrier, server: server}
}

func (a *application) run(ctx context.Context) error {
	listener, err := net.Listen("tcp", a.cfg.listenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", a.cfg.listenAddr, err)
	}
	defer a.server.Close()
	serverErr := make(chan error, 1)
	go func() {
		err := a.server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()
	slog.Info("supervisor listening", "address", a.cfg.listenAddr, "revision", a.cfg.revision)

	if err := a.lease.acquire(ctx); err != nil {
		return fmt.Errorf("acquire writer lease: %w", err)
	}
	slog.Info("writer lease acquired", "owner", a.cfg.instanceID)
	a.lease.clearTakeover(ctx)

	leaseCtx, cancelLease := context.WithCancel(context.Background())
	defer cancelLease()
	leaseEvents := a.lease.maintain(leaseCtx)

	processCtx, cancelProcesses := context.WithCancel(context.Background())
	defer cancelProcesses()
	startupDone := make(chan error, 1)
	go func() { startupDone <- a.startDataPlane(processCtx) }()

	var startupErr error
	var startupEvent *leaseEvent
	select {
	case startupErr = <-startupDone:
	case <-ctx.Done():
		cancelProcesses()
		startupErr = <-startupDone
		if startupErr == nil {
			startupErr = ctx.Err()
		}
	case event := <-leaseEvents:
		startupEvent = &event
		cancelProcesses()
		startupErr = <-startupDone
		if startupErr == nil {
			startupErr = event.err
		}
	case err := <-serverErr:
		cancelProcesses()
		<-startupDone
		startupErr = fmt.Errorf("HTTP server exited: %w", err)
	}
	if startupErr != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if startupEvent != nil && !startupEvent.stillOwner {
			cancelLease()
			a.failClosed()
		} else {
			a.gracefulShutdown(shutdownCtx, cancelLease)
		}
		return startupErr
	}
	a.state.ready.Store(true)
	slog.Info("vaultwarden is ready")

	var event leaseEvent
	select {
	case <-ctx.Done():
		event = leaseEvent{err: ctx.Err(), stillOwner: a.lease.valid()}
	case event = <-leaseEvents:
	case <-a.litestream.done:
		event = leaseEvent{err: fmt.Errorf("litestream exited: %w", a.litestream.waitError()), stillOwner: a.lease.valid()}
	case <-a.vaultwarden.done:
		event = leaseEvent{err: fmt.Errorf("vaultwarden exited: %w", a.vaultwarden.waitError()), stillOwner: a.lease.valid()}
	case err := <-serverErr:
		event = leaseEvent{err: fmt.Errorf("HTTP server exited: %w", err), stillOwner: a.lease.valid()}
	}
	a.state.ready.Store(false)
	slog.Warn("supervisor stopping", "reason", event.err, "still_owner", event.stillOwner)

	if !event.stillOwner {
		cancelLease()
		a.failClosed()
		return event.err
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
	defer cancel()
	a.gracefulShutdown(shutdownCtx, cancelLease)
	return event.err
}

func (a *application) startDataPlane(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(a.cfg.databasePath), 0o700); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(a.cfg.litestreamSocket), 0o755); err != nil {
		return fmt.Errorf("create litestream socket directory: %w", err)
	}

	litestream, err := startProcess(ctx, "litestream", a.cfg.litestreamBin,
		[]string{"replicate", "-config", a.cfg.litestreamConfig, "-restore-if-db-not-exists"}, []string{
			"DATABASE_PATH=" + a.cfg.databasePath,
			"LITESTREAM_REPLICA_URL=" + a.cfg.replicaURL,
			"LITESTREAM_SOCKET=" + a.cfg.litestreamSocket,
		})
	if err != nil {
		return err
	}
	a.litestream = litestream
	if err := waitForSocket(ctx, a.cfg.litestreamSocket, a.cfg.startupTimeout, litestream.done); err != nil {
		litestream.stop(2 * time.Second)
		return err
	}

	upstreamHost, upstreamPort, err := net.SplitHostPort(a.cfg.upstreamURL.Host)
	if err != nil {
		return fmt.Errorf("parse upstream address: %w", err)
	}
	vaultwarden, err := startProcess(ctx, "vaultwarden", a.cfg.vaultwardenBin, nil, []string{
		"ROCKET_ADDRESS=" + upstreamHost,
		"ROCKET_PORT=" + upstreamPort,
		"DATABASE_URL=sqlite://" + a.cfg.databasePath,
		"ENABLE_DB_WAL=true",
	})
	if err != nil {
		litestream.stop(2 * time.Second)
		return err
	}
	a.vaultwarden = vaultwarden
	if err := waitForHTTP(ctx, a.cfg.upstreamURL.String()+"/alive", a.cfg.startupTimeout, vaultwarden.done); err != nil {
		vaultwarden.stop(2 * time.Second)
		litestream.stop(2 * time.Second)
		return err
	}
	return nil
}

func waitForSocket(ctx context.Context, path string, timeout time.Duration, processDone <-chan struct{}) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-processDone:
			return fmt.Errorf("process exited during startup")
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for socket %s", path)
		case <-ticker.C:
			conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return nil
			}
		}
	}
}

func waitForHTTP(ctx context.Context, endpoint string, timeout time.Duration, processDone <-chan struct{}) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Timeout: time.Second}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-processDone:
			return fmt.Errorf("process exited during startup")
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for %s", endpoint)
		case <-ticker.C:
			resp, err := client.Get(endpoint)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					return nil
				}
			}
		}
	}
}

func (a *application) gracefulShutdown(ctx context.Context, cancelLease context.CancelFunc) {
	a.state.ready.Store(false)
	cancelLease()
	drainCtx, cancelDrain := context.WithTimeout(ctx, 2*time.Second)
	_ = a.server.Shutdown(drainCtx)
	cancelDrain()
	_ = a.server.Close()
	if a.vaultwarden != nil {
		a.vaultwarden.stop(2 * time.Second)
	}
	if a.litestream != nil && a.lease.valid() {
		syncCtx, cancelSync := context.WithTimeout(ctx, min(a.cfg.syncTimeout, 2*time.Second))
		if err := a.barrier.sync(syncCtx); err != nil {
			slog.Error("final replication sync failed", "error", err)
		}
		cancelSync()
	}
	if a.litestream != nil {
		a.litestream.stop(2 * time.Second)
	}
	releaseCtx, cancelRelease := context.WithTimeout(ctx, time.Second)
	if err := a.lease.release(releaseCtx); err != nil {
		slog.Error("writer lease release failed", "error", err)
	}
	cancelRelease()
}

func (a *application) failClosed() {
	a.state.ready.Store(false)
	_ = a.server.Close()
	if a.vaultwarden != nil {
		a.vaultwarden.kill()
	}
	if a.litestream != nil {
		a.litestream.kill()
	}
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	app := newApplication(cfg, newGCSStore())
	if err := app.run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("supervisor exited", "error", err)
		os.Exit(1)
	}
}
