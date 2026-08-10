package governance

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
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
