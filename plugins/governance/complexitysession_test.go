package governance

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/kvstore"
)

func TestResolveSessionIDPrecedence(t *testing.T) {
	ctx := complexityHarnessContext(schemas.ClaudeCLI.String(), map[string]string{
		claudeCodeSessionIDHeader: " native-session ",
	})
	ctx.SetValue(schemas.BifrostContextKeySessionID, " explicit-session ")
	req := complexitySessionChatRequest("Be concise", "Explain vector clocks")

	tests := []struct {
		name        string
		sources     []string
		wantID      string
		wantSource  string
		wantPresent bool
	}{
		{
			name: "header_wins_regardless_of_config_order",
			sources: []string{
				configstore.ComplexitySessionIdentityFingerprint,
				configstore.ComplexitySessionIdentityHarness,
				configstore.ComplexitySessionIdentityHeader,
			},
			wantID:      "explicit-session",
			wantSource:  configstore.ComplexitySessionIdentityHeader,
			wantPresent: true,
		},
		{
			name: "harness_wins_when_header_is_disabled",
			sources: []string{
				configstore.ComplexitySessionIdentityFingerprint,
				configstore.ComplexitySessionIdentityHarness,
			},
			wantID:      "native-session",
			wantSource:  configstore.ComplexitySessionIdentityHarness,
			wantPresent: true,
		},
		{
			name:        "fingerprint_runs_only_when_enabled",
			sources:     []string{configstore.ComplexitySessionIdentityFingerprint},
			wantSource:  configstore.ComplexitySessionIdentityFingerprint,
			wantPresent: true,
		},
		{
			name:    "no_enabled_sources",
			sources: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotSource, gotPresent := resolveSessionID(ctx, req, tt.sources)
			assert.Equal(t, tt.wantPresent, gotPresent)
			assert.Equal(t, tt.wantSource, gotSource)
			if tt.wantID != "" {
				assert.Equal(t, tt.wantID, gotID)
			} else if tt.wantPresent {
				assert.Len(t, gotID, 64)
			} else {
				assert.Empty(t, gotID)
			}
		})
	}
}

func TestResolveSessionIDHarnessSources(t *testing.T) {
	tests := []struct {
		name        string
		userAgent   string
		headers     map[string]string
		wantID      string
		wantPresent bool
	}{
		{
			name:      "claude_code_header",
			userAgent: schemas.ClaudeCLI.String(),
			headers: map[string]string{
				claudeCodeSessionIDHeader: " claude-session ",
			},
			wantID:      "claude-session",
			wantPresent: true,
		},
		{
			name:      "codex_cli_metadata",
			userAgent: schemas.CodexCLI.String(),
			headers: map[string]string{
				codexTurnMetadataHeader: `{"request_kind":"turn","session_id":" codex-session "}`,
			},
			wantID:      "codex-session",
			wantPresent: true,
		},
		{
			name:      "codex_desktop_metadata",
			userAgent: schemas.CodexDesktop.String(),
			headers: map[string]string{
				codexTurnMetadataHeader: `{"session_id":"desktop-session"}`,
			},
			wantID:      "desktop-session",
			wantPresent: true,
		},
		{
			name:      "generic_client_cannot_claim_claude_session",
			userAgent: "generic-client/1.0",
			headers: map[string]string{
				claudeCodeSessionIDHeader: "spoofed-session",
			},
		},
		{
			name:      "generic_client_cannot_claim_codex_session",
			userAgent: "generic-client/1.0",
			headers: map[string]string{
				codexTurnMetadataHeader: `{"session_id":"spoofed-session"}`,
			},
		},
		{
			name:      "malformed_codex_metadata",
			userAgent: schemas.CodexCLI.String(),
			headers: map[string]string{
				codexTurnMetadataHeader: "{",
			},
		},
		{
			name:      "non_string_codex_session",
			userAgent: schemas.CodexCLI.String(),
			headers: map[string]string{
				codexTurnMetadataHeader: `{"session_id":123}`,
			},
		},
		{
			name:      "blank_claude_session",
			userAgent: schemas.ClaudeCLI.String(),
			headers: map[string]string{
				claudeCodeSessionIDHeader: "   ",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := complexityHarnessContext(tt.userAgent, tt.headers)
			gotID, gotSource, gotPresent := resolveSessionID(ctx, nil, []string{configstore.ComplexitySessionIdentityHarness})
			assert.Equal(t, tt.wantPresent, gotPresent)
			assert.Equal(t, tt.wantID, gotID)
			if tt.wantPresent {
				assert.Equal(t, configstore.ComplexitySessionIdentityHarness, gotSource)
			} else {
				assert.Empty(t, gotSource)
			}
		})
	}
}

func TestResolveSessionIDMalformedHarnessFallsThrough(t *testing.T) {
	ctx := complexityHarnessContext(schemas.CodexCLI.String(), map[string]string{
		codexTurnMetadataHeader: "{",
	})
	req := complexitySessionChatRequest("Be concise", "Explain vector clocks")

	id, source, ok := resolveSessionID(ctx, req, []string{
		configstore.ComplexitySessionIdentityHarness,
		configstore.ComplexitySessionIdentityFingerprint,
	})

	require.True(t, ok)
	assert.Len(t, id, 64)
	assert.Equal(t, configstore.ComplexitySessionIdentityFingerprint, source)
}

func TestResolveSessionIDBlankHeaderFallsThrough(t *testing.T) {
	ctx := complexityHarnessContext(schemas.ClaudeCLI.String(), map[string]string{
		claudeCodeSessionIDHeader: "native-session",
	})
	ctx.SetValue(schemas.BifrostContextKeySessionID, " ")

	id, source, ok := resolveSessionID(ctx, nil, []string{
		configstore.ComplexitySessionIdentityHeader,
		configstore.ComplexitySessionIdentityHarness,
	})

	require.True(t, ok)
	assert.Equal(t, "native-session", id)
	assert.Equal(t, configstore.ComplexitySessionIdentityHarness, source)
}

func TestComplexitySessionFingerprintStablePrefix(t *testing.T) {
	ctx := complexityHarnessContext(schemas.ClaudeCLI.String(), nil)
	systemWithWrapper := "Be concise. <system-reminder>Repository context</system-reminder>"
	firstReq := complexitySessionChatRequest(systemWithWrapper, "Explain vector clocks")
	laterReq := complexitySessionChatRequest(
		"Be concise.",
		"Explain vector clocks",
		"Compare them to Lamport clocks",
	)

	firstID, firstOK := complexitySessionFingerprint(ctx, firstReq)
	laterID, laterOK := complexitySessionFingerprint(ctx, laterReq)

	require.True(t, firstOK)
	require.True(t, laterOK)
	assert.Equal(t, firstID, laterID, "later turns and stripped harness context must not change the stable prefix")
	assert.Equal(t, systemWithWrapper, *firstReq.ChatRequest.Input[0].Content.ContentStr, "fingerprinting must not mutate provider-bound input")

	changedSystemID, ok := complexitySessionFingerprint(ctx, complexitySessionChatRequest("Be detailed", "Explain vector clocks"))
	require.True(t, ok)
	assert.NotEqual(t, firstID, changedSystemID)

	changedFirstTurnID, ok := complexitySessionFingerprint(ctx, complexitySessionChatRequest("Be concise", "Explain hybrid clocks"))
	require.True(t, ok)
	assert.NotEqual(t, firstID, changedFirstTurnID)
}

func TestComplexitySessionFingerprintSkipsCodexBackgroundRequest(t *testing.T) {
	ctx := complexityHarnessContext(schemas.CodexCLI.String(), map[string]string{
		codexTurnMetadataHeader: `{"request_kind":"compaction","session_id":"codex-session"}`,
	})

	id, source, ok := resolveSessionID(
		ctx,
		complexitySessionChatRequest("Be concise", "Compact the conversation"),
		[]string{configstore.ComplexitySessionIdentityFingerprint},
	)

	assert.False(t, ok)
	assert.Empty(t, id)
	assert.Empty(t, source)
}

func TestCodexBackgroundKindSurvivesMalformedSessionID(t *testing.T) {
	ctx := complexityHarnessContext(schemas.CodexCLI.String(), map[string]string{
		codexTurnMetadataHeader: `{"request_kind":"compaction","session_id":123}`,
	})

	input, ok := buildComplexityInput(ctx, complexitySessionChatRequest("Be concise", "Compact the conversation"))

	assert.False(t, ok)
	assert.Empty(t, input)
}

func TestBuildComplexitySessionKeyTenantIsolation(t *testing.T) {
	const (
		tenantID  = "tenant-secret-value"
		sessionID = "session-private-value"
	)

	key, ok := buildComplexitySessionKey(tenantID, sessionID)
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(key, complexitySessionKeyPrefix))
	assert.NotContains(t, key, tenantID)
	assert.NotContains(t, key, sessionID)

	sameKey, ok := buildComplexitySessionKey(tenantID, sessionID)
	require.True(t, ok)
	assert.Equal(t, key, sameKey)

	otherTenantKey, ok := buildComplexitySessionKey("other-tenant", sessionID)
	require.True(t, ok)
	assert.NotEqual(t, key, otherTenantKey)

	otherSessionKey, ok := buildComplexitySessionKey(tenantID, "other-session")
	require.True(t, ok)
	assert.NotEqual(t, key, otherSessionKey)
}

func TestBuildComplexitySessionKeyRejectsBlankIdentity(t *testing.T) {
	for _, tt := range []struct {
		name      string
		tenantID  string
		sessionID string
	}{
		{name: "blank_tenant", tenantID: " ", sessionID: "session"},
		{name: "blank_session", tenantID: "tenant", sessionID: "\t"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			key, ok := buildComplexitySessionKey(tt.tenantID, tt.sessionID)
			assert.False(t, ok)
			assert.Empty(t, key)
		})
	}
}

func TestKVSessionStoreRoundTripAndOwnership(t *testing.T) {
	sessionStore, _ := newTestKVSessionStore(t)
	key := testComplexitySessionKey(t, "tenant", "round-trip")
	decidedAt := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	lastSeenAt := decidedAt.Add(time.Minute)
	record := &SessionComplexityRecord{
		Tier:            "complex",
		DecidedAt:       decidedAt,
		RuleID:          "rule-1",
		SwitchCount:     2,
		PendingTier:     "medium",
		PendingTurns:    1,
		PendingMinScore: 0.91,
		RouteObservations: map[string]SessionRouteObservation{
			"route-1": {
				CachedReadTokens: 4096,
				CacheObserved:    true,
				LastSeenAt:       lastSeenAt,
			},
		},
	}
	want := cloneSessionComplexityRecord(record)

	created, err := sessionStore.Create(context.Background(), key, record, time.Hour)
	require.NoError(t, err)
	require.True(t, created)

	// Mutating the input after Create must not mutate stored state.
	record.Tier = "simple"
	record.RouteObservations["route-1"] = SessionRouteObservation{CachedReadTokens: 1}

	got, found, err := sessionStore.Get(context.Background(), key, time.Hour)
	require.NoError(t, err)
	require.True(t, found)
	// The store stamps RefreshedAt itself, so it is asserted separately and
	// cleared before the structural comparison.
	assert.False(t, got.RefreshedAt.IsZero(), "Create should stamp the refresh clock")
	got.RefreshedAt = time.Time{}
	assert.Equal(t, want, got)

	// Mutating a returned record must not mutate stored state either.
	got.PendingTurns = 99
	got.RouteObservations["route-1"] = SessionRouteObservation{CachedReadTokens: 2}

	gotAgain, found, err := sessionStore.Get(context.Background(), key, time.Hour)
	require.NoError(t, err)
	require.True(t, found)
	gotAgain.RefreshedAt = time.Time{}
	assert.Equal(t, want, gotAgain)
}

func TestKVSessionStoreCreateIsAtomic(t *testing.T) {
	sessionStore, _ := newTestKVSessionStore(t)
	key := testComplexitySessionKey(t, "tenant", "create-race")

	const contenders = 50
	var createdCount atomic.Int32
	errCh := make(chan error, contenders)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			created, err := sessionStore.Create(context.Background(), key, &SessionComplexityRecord{
				Tier:   "complex",
				RuleID: "rule-1",
			}, time.Hour)
			if err != nil {
				errCh <- err
				return
			}
			if created {
				createdCount.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("create failed: %v", err)
	}
	assert.EqualValues(t, 1, createdCount.Load())
}

func TestKVSessionStoreUpdateIsAtomic(t *testing.T) {
	sessionStore, _ := newTestKVSessionStore(t)
	key := testComplexitySessionKey(t, "tenant", "update-race")
	created, err := sessionStore.Create(context.Background(), key, &SessionComplexityRecord{
		Tier:              "complex",
		RuleID:            "rule-1",
		RouteObservations: map[string]SessionRouteObservation{},
	}, time.Hour)
	require.NoError(t, err)
	require.True(t, created)

	const updates = 100
	errCh := make(chan error, updates)
	var wg sync.WaitGroup
	for range updates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, found, err := sessionStore.Update(context.Background(), key, time.Hour, func(record *SessionComplexityRecord) error {
				record.PendingTurns++
				return nil
			})
			if err != nil {
				errCh <- err
				return
			}
			if !found {
				errCh <- kvstore.ErrNotFound
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("update failed: %v", err)
	}

	record, found, err := sessionStore.Get(context.Background(), key, time.Hour)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, updates, record.PendingTurns)
}

func TestKVSessionStoreUpdateFailureDoesNotMutate(t *testing.T) {
	sessionStore, _ := newTestKVSessionStore(t)
	key := testComplexitySessionKey(t, "tenant", "failed-update")
	created, err := sessionStore.Create(context.Background(), key, &SessionComplexityRecord{
		Tier:         "complex",
		RuleID:       "rule-1",
		PendingTurns: 1,
	}, time.Hour)
	require.NoError(t, err)
	require.True(t, created)

	wantErr := errors.New("policy rejected update")
	_, found, err := sessionStore.Update(context.Background(), key, time.Hour, func(record *SessionComplexityRecord) error {
		record.PendingTurns = 99
		return wantErr
	})
	require.ErrorIs(t, err, wantErr)
	require.True(t, found)

	record, found, err := sessionStore.Get(context.Background(), key, time.Hour)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 1, record.PendingTurns)
}

func TestKVSessionStoreSlidingTTL(t *testing.T) {
	sessionStore, _ := newTestKVSessionStore(t)
	key := testComplexitySessionKey(t, "tenant", "sliding-ttl")
	const ttl = 250 * time.Millisecond

	created, err := sessionStore.Create(context.Background(), key, &SessionComplexityRecord{
		Tier:   "complex",
		RuleID: "rule-1",
	}, ttl)
	require.NoError(t, err)
	require.True(t, created)

	time.Sleep(150 * time.Millisecond)
	_, found, err := sessionStore.Get(context.Background(), key, ttl)
	require.NoError(t, err)
	require.True(t, found, "record should exist before its original deadline")

	time.Sleep(150 * time.Millisecond)
	_, found, err = sessionStore.Get(context.Background(), key, ttl)
	require.NoError(t, err)
	require.True(t, found, "successful Get should extend the deadline")

	// Expiry is ttl plus one refresh interval of headroom, so an abandoned
	// session lingers a little past the configured window rather than expiring
	// early — the direction that matters, since expiring early would drop a live
	// conversation's tier mid-way.
	time.Sleep(ttl + complexitySessionRefreshInterval(ttl) + 100*time.Millisecond)
	_, found, err = sessionStore.Get(context.Background(), key, ttl)
	require.NoError(t, err)
	assert.False(t, found, "record should expire once its TTL is no longer refreshed")
}

// The refresh is coarse on purpose: sliding the expiry on every read makes each
// read a write, and on a replicated backend each write is a cluster broadcast.
// A chatty conversation must not cost one broadcast per turn.
func TestKVSessionStoreGetDoesNotWriteOnEveryRead(t *testing.T) {
	sessionStore, store := newTestKVSessionStore(t)
	_, targetKV := newTestKVSessionStore(t)
	delegate := &forwardingKVDelegate{target: targetKV}

	key := testComplexitySessionKey(t, "tenant", "read-amplification")
	_, err := sessionStore.Create(context.Background(), key, &SessionComplexityRecord{Tier: "complex"}, time.Hour)
	require.NoError(t, err)

	// Attached after Create so only the reads below are counted.
	store.SetDelegate(delegate)
	for i := 0; i < 50; i++ {
		_, found, err := sessionStore.Get(context.Background(), key, time.Hour)
		require.NoError(t, err)
		require.True(t, found)
	}

	assert.Zero(t, delegate.Sets(), "reads inside the refresh interval must not replicate")
}

// The window must still slide for a long-lived conversation, or a session that
// stays busy past its ttl would expire underneath itself.
func TestKVSessionStoreGetRefreshesOnceTheIntervalElapses(t *testing.T) {
	sessionStore, _ := newTestKVSessionStore(t)
	key := testComplexitySessionKey(t, "tenant", "interval-elapsed")
	const ttl = 400 * time.Millisecond

	_, err := sessionStore.Create(context.Background(), key, &SessionComplexityRecord{Tier: "complex"}, ttl)
	require.NoError(t, err)

	before, found, err := sessionStore.Get(context.Background(), key, ttl)
	require.NoError(t, err)
	require.True(t, found)

	// Past one refresh interval (ttl/4 = 100ms), so the next read slides it.
	time.Sleep(complexitySessionRefreshInterval(ttl) + 50*time.Millisecond)
	after, found, err := sessionStore.Get(context.Background(), key, ttl)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, after.RefreshedAt.After(before.RefreshedAt), "the window never slid")
}

func TestKVSessionStoreRegistersReplicationDecoder(t *testing.T) {
	source, sourceKV := newTestKVSessionStore(t)
	target, targetKV := newTestKVSessionStore(t)
	delegate := &forwardingKVDelegate{target: targetKV}
	sourceKV.SetDelegate(delegate)

	key := testComplexitySessionKey(t, "tenant", "replicated")
	want := &SessionComplexityRecord{
		Tier:   "complex",
		RuleID: "rule-1",
		RouteObservations: map[string]SessionRouteObservation{
			"route-1": {CachedReadTokens: 2048, CacheObserved: true},
		},
	}
	created, err := source.Create(context.Background(), key, want, time.Hour)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, delegate.Err())

	replicatedValue, err := targetKV.Get(key)
	require.NoError(t, err)
	assert.IsType(t, &SessionComplexityRecord{}, replicatedValue)

	got, found, err := target.Get(context.Background(), key, time.Hour)
	require.NoError(t, err)
	require.True(t, found)
	assert.False(t, got.RefreshedAt.IsZero(), "the replicated record should carry the refresh clock")
	got.RefreshedAt = time.Time{}
	assert.Equal(t, want, got)
}

func TestKVSessionStoreRejectsInvalidReplicatedRecord(t *testing.T) {
	sessionStore, store := newTestKVSessionStore(t)
	key := testComplexitySessionKey(t, "tenant", "invalid-replica")
	require.NoError(t, store.SetRemote(key, []byte("{"), time.Now().UnixNano(), 0))

	_, found, err := sessionStore.Get(context.Background(), key, time.Hour)
	require.ErrorIs(t, err, errInvalidComplexitySession)
	assert.True(t, found)
}

func TestKVSessionStoreDeleteAndValidation(t *testing.T) {
	sessionStore, _ := newTestKVSessionStore(t)
	key := testComplexitySessionKey(t, "tenant", "delete")
	record := &SessionComplexityRecord{Tier: "complex", RuleID: "rule-1"}

	created, err := sessionStore.Create(context.Background(), key, record, time.Hour)
	require.NoError(t, err)
	require.True(t, created)
	deleted, err := sessionStore.Delete(context.Background(), key)
	require.NoError(t, err)
	assert.True(t, deleted)
	deleted, err = sessionStore.Delete(context.Background(), key)
	require.NoError(t, err)
	assert.False(t, deleted)

	_, err = sessionStore.Create(context.Background(), key, record, 0)
	require.ErrorIs(t, err, errInvalidComplexitySessionTTL)
	_, err = sessionStore.Create(context.Background(), key, nil, time.Hour)
	require.ErrorIs(t, err, errNilComplexitySessionRecord)
	_, _, err = sessionStore.Update(context.Background(), key, time.Hour, nil)
	require.ErrorIs(t, err, errNilComplexitySessionUpdater)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = sessionStore.Create(canceled, key, record, time.Hour)
	require.ErrorIs(t, err, context.Canceled)
	_, err = sessionStore.Status(nil)
	require.ErrorIs(t, err, errNilComplexitySessionContext)
}

func TestKVSessionStoreStatus(t *testing.T) {
	sessionStore, store := newTestKVSessionStore(t)

	status, err := sessionStore.Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, SessionStoreStatus{
		Backend:               complexitySessionStoreBackendKV,
		ReplicationConfigured: false,
		AtomicAcrossReplicas:  true,
	}, status)

	_, targetKV := newTestKVSessionStore(t)
	store.SetDelegate(&forwardingKVDelegate{target: targetKV})
	status, err = sessionStore.Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, SessionStoreStatus{
		Backend:               complexitySessionStoreBackendKV,
		ReplicationConfigured: true,
		AtomicAcrossReplicas:  false,
	}, status)
}

func newTestKVSessionStore(t *testing.T) (*kvSessionStore, *kvstore.Store) {
	t.Helper()
	store, err := kvstore.New(kvstore.Config{CleanupInterval: time.Hour})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})
	sessionStore, err := newKVSessionStore(store)
	require.NoError(t, err)
	return sessionStore, store
}

func testComplexitySessionKey(t *testing.T, tenantID, sessionID string) string {
	t.Helper()
	key, ok := buildComplexitySessionKey(tenantID, sessionID)
	require.True(t, ok)
	return key
}

type forwardingKVDelegate struct {
	target *kvstore.Store
	mu     sync.Mutex
	err    error
	sets   int
}

func (d *forwardingKVDelegate) OnSet(key string, valueJSON []byte, writtenAt int64, expiresAt int64) {
	d.mu.Lock()
	d.sets++
	d.mu.Unlock()
	d.recordError(d.target.SetRemote(key, valueJSON, writtenAt, expiresAt))
}

// Sets reports how many replication messages this delegate has been asked to
// send, which is the cost a read path must not incur per request.
func (d *forwardingKVDelegate) Sets() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sets
}

func (d *forwardingKVDelegate) OnDelete(key string, deletedAt int64) {
	d.recordError(d.target.DeleteRemote(key, deletedAt))
}

func (d *forwardingKVDelegate) recordError(err error) {
	if err == nil {
		return
	}
	d.mu.Lock()
	if d.err == nil {
		d.err = err
	}
	d.mu.Unlock()
}

func (d *forwardingKVDelegate) Err() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

func complexitySessionChatRequest(systemText string, userTexts ...string) *schemas.BifrostRequest {
	messages := make([]schemas.ChatMessage, 0, len(userTexts)+1)
	if systemText != "" {
		messages = append(messages, schemas.ChatMessage{
			Role:    schemas.ChatMessageRoleSystem,
			Content: complexityChatString(systemText),
		})
	}
	for _, userText := range userTexts {
		messages = append(messages, schemas.ChatMessage{
			Role:    schemas.ChatMessageRoleUser,
			Content: complexityChatString(userText),
		})
	}
	return &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{Input: messages},
	}
}
