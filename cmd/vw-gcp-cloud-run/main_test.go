package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	mu      sync.Mutex
	objects map[string]storedObject
	nextGen int64
}

func newMemoryStore() *memoryStore {
	return &memoryStore{objects: make(map[string]storedObject), nextGen: 1}
}

func objectKey(bucket, name string) string { return bucket + "/" + name }

func (s *memoryStore) get(_ context.Context, bucket, name string) (storedObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[objectKey(bucket, name)]
	if !ok {
		return storedObject{}, errNotFound
	}
	now := time.Now()
	obj.serverTime = now
	return obj, nil
}

func (s *memoryStore) put(_ context.Context, bucket, name string, data []byte, generation int64) (storedObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := objectKey(bucket, name)
	current, exists := s.objects[key]
	if generation == 0 && exists {
		return storedObject{}, errPrecondition
	}
	if generation != 0 && (!exists || current.generation != generation) {
		return storedObject{}, errPrecondition
	}
	now := time.Now()
	obj := storedObject{data: append([]byte(nil), data...), generation: s.nextGen, updated: now, serverTime: now}
	s.nextGen++
	s.objects[key] = obj
	return obj, nil
}

func (s *memoryStore) delete(_ context.Context, bucket, name string, generation int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := objectKey(bucket, name)
	current, exists := s.objects[key]
	if !exists {
		return nil
	}
	if current.generation != generation {
		return errPrecondition
	}
	delete(s.objects, key)
	return nil
}

func testLease(store objectStore, owner string) *leaseManager {
	return &leaseManager{
		store: store, bucket: "bucket", leaseObject: "control/lease.json", takeoverObject: "control/takeover.json",
		owner: owner, revision: "vaultwarden-1-abc", ttl: time.Minute, renewInterval: time.Second, pollInterval: time.Millisecond,
	}
}

func TestLeaseGenerationPreventsTwoOwners(t *testing.T) {
	store := newMemoryStore()
	first := testLease(store, "first")
	if err := first.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}

	second := testLease(store, "second")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := second.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquire error = %v, want deadline exceeded", err)
	}
	if !first.valid() {
		t.Fatal("first owner unexpectedly lost its lease")
	}
	if second.valid() {
		t.Fatal("second owner unexpectedly acquired the lease")
	}
}

func TestLeasePreconditionFailureReportsOwnershipLoss(t *testing.T) {
	store := newMemoryStore()
	lease := testLease(store, "first")
	if err := lease.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	obj := store.objects[objectKey("bucket", "control/lease.json")]
	obj.generation++
	store.objects[objectKey("bucket", "control/lease.json")] = obj
	store.mu.Unlock()

	event := lease.renewAndCheck(t.Context())
	if event == nil || event.stillOwner {
		t.Fatalf("event = %#v, want ownership-loss event", event)
	}
	if lease.valid() {
		t.Fatal("lease remained valid after a generation mismatch")
	}
}

func TestExpiredLocalLeaseRevalidatesSameGeneration(t *testing.T) {
	store := newMemoryStore()
	lease := testLease(store, "first")
	if err := lease.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}

	lease.mu.Lock()
	lease.lastRenewal = time.Now().Add(-lease.localValidityWindow())
	lease.mu.Unlock()
	if err := lease.ensureValid(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !lease.valid() {
		t.Fatal("lease did not become valid after generation-checked renewal")
	}
}

func TestExpiredLocalLeaseRejectsRequestAfterGenerationLoss(t *testing.T) {
	store := newMemoryStore()
	lease := testLease(store, "first")
	if err := lease.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}

	lease.mu.Lock()
	lease.lastRenewal = time.Now().Add(-lease.localValidityWindow())
	lease.mu.Unlock()
	store.mu.Lock()
	key := objectKey("bucket", "control/lease.json")
	obj := store.objects[key]
	obj.generation++
	store.objects[key] = obj
	store.mu.Unlock()

	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamCalled = true
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	state := &proxyState{lease: lease}
	state.ready.Store(true)
	handler := newProxy(state, target, &syncBarrier{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/config", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if upstreamCalled {
		t.Fatal("request reached upstream after lease generation changed")
	}
}

func TestExpiredLeaseCanBeReplaced(t *testing.T) {
	store := newMemoryStore()
	first := testLease(store, "first")
	if err := first.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	key := objectKey("bucket", "control/lease.json")
	obj := store.objects[key]
	obj.updated = time.Now().Add(-2 * first.ttl)
	store.objects[key] = obj
	store.mu.Unlock()

	second := testLease(store, "second")
	if err := second.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !second.valid() {
		t.Fatal("second owner did not receive a valid replacement lease")
	}
	stored, err := store.get(t.Context(), "bucket", "control/lease.json")
	if err != nil {
		t.Fatal(err)
	}
	var holder leasePayload
	if err := json.Unmarshal(stored.data, &holder); err != nil {
		t.Fatal(err)
	}
	if holder.Owner != "second" {
		t.Fatalf("lease owner = %q, want second", holder.Owner)
	}
}

func TestOlderRevisionCannotReacquireDuringTakeover(t *testing.T) {
	store := newMemoryStore()
	takeover, _ := json.Marshal(takeoverPayload{Version: 1, Requester: "new", Revision: "vaultwarden-2-new"})
	if _, err := store.put(t.Context(), "bucket", "control/takeover.json", takeover, 0); err != nil {
		t.Fatal(err)
	}

	old := testLease(store, "old")
	old.revision = "vaultwarden-1-old"
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := old.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("old revision acquire error = %v, want deadline exceeded", err)
	}

	newRevision := testLease(store, "new")
	newRevision.revision = "vaultwarden-2-new"
	if err := newRevision.acquire(t.Context()); err != nil {
		t.Fatalf("new revision failed to acquire: %v", err)
	}
}

func TestMutationResponseWaitsForRemoteTXID(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "litestream.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	syncReached := make(chan struct{}, 1)
	ipc := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sync" {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Wait bool `json:"wait"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !request.Wait {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		syncReached <- struct{}{}
		_ = json.NewEncoder(w).Encode(map[string]any{"txid": 7, "replicated_txid": 7})
	})}
	go ipc.Serve(listener)
	t.Cleanup(func() { _ = ipc.Close() })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)

	store := newMemoryStore()
	lease := testLease(store, "first")
	if err := lease.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	state := &proxyState{lease: lease}
	state.ready.Store(true)
	proxy := httptest.NewServer(newProxy(state, target, &syncBarrier{socket: socket, databasePath: "/db.sqlite3", timeout: time.Second}))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/api/ciphers", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	select {
	case <-syncReached:
	default:
		t.Fatal("mutation response returned without reaching the remote sync barrier")
	}
}

func TestMutationResponseFailsWhenRemoteTXIDIsBehind(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "litestream.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ipc := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"txid": 8, "replicated_txid": 7})
	})}
	go ipc.Serve(listener)
	t.Cleanup(func() { _ = ipc.Close() })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)

	store := newMemoryStore()
	lease := testLease(store, "first")
	if err := lease.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	state := &proxyState{lease: lease}
	state.ready.Store(true)
	proxy := httptest.NewServer(newProxy(state, target, &syncBarrier{socket: socket, databasePath: "/db.sqlite3", timeout: time.Second}))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/api/ciphers", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestRevisionNumber(t *testing.T) {
	if got := revisionNumber("vaultwarden-42-abc"); got != 42 {
		t.Fatalf("revisionNumber() = %d, want 42", got)
	}
	if got := revisionNumber("local"); got != -1 {
		t.Fatalf("revisionNumber(local) = %d, want -1", got)
	}
}

func TestMergedEnvReplacesSupervisorOwnedValues(t *testing.T) {
	got := mergedEnv(
		[]string{"DATABASE_URL=sqlite:///old.sqlite3", "DOMAIN=https://example.com"},
		[]string{"DATABASE_URL=sqlite:///local/db.sqlite3"},
	)
	want := []string{"DOMAIN=https://example.com", "DATABASE_URL=sqlite:///local/db.sqlite3"}
	if len(got) != len(want) {
		t.Fatalf("mergedEnv() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mergedEnv() = %v, want %v", got, want)
		}
	}
}
