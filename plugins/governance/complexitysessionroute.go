package governance

import (
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/plugins/governance/complexity"
)

// complexitySessionGlobalTenant namespaces sessions on deployments that route
// without virtual keys. Session ids from different callers then share one
// namespace, which is harmless for opaque ids but does mean two callers whose
// conversations open identically can share a fingerprint-derived session.
const complexitySessionGlobalTenant = "global"

// complexitySessionContextKey carries the resolved session across the request so
// the response path can record what the provider reported without re-running
// identity resolution, which needs the request body it no longer has.
const complexitySessionContextKey schemas.BifrostContextKey = "bf-governance-complexity-session"

const (
	// maxSessionRouteObservations bounds retained per-route history. A session
	// that cycles through fallbacks would otherwise grow this map without limit,
	// and the whole record is rewritten — and replicated — on every write.
	maxSessionRouteObservations = 8
	// sessionObservationChangeRatio is how much the cached-token count must move
	// before it is worth a write. Cache sizes drift a little every turn, and
	// persisting each drift would put a cluster broadcast back on every request.
	sessionObservationChangeRatio = 0.1
)

// complexitySessionState is the per-request session context, resolved once
// before routing so both the routing engine and the classification closure work
// from the same identity.
type complexitySessionState struct {
	// ID is the resolved conversation identity, empty when nothing identified it.
	ID string
	// Source names which rung of the identity ladder produced ID.
	Source string
	// Key is the tenant-namespaced store key, empty when ID is empty.
	Key string
	// TTL is the sliding idle window from configuration.
	TTL time.Duration
}

// resolveComplexitySessionState resolves the conversation identity for this
// request. It returns nil when session behaviour is off, when no store is
// attached, or when nothing identified the conversation — all of which mean
// "classify this turn normally", the pre-session behaviour.
//
// Resolution runs eagerly because the routing engine needs the id to make target
// selection sticky, and the ladder is only context reads plus, at most, a hash
// of the conversation prefix. The store lookup stays lazy: it happens inside the
// classification closure, so a request whose rules never mention complexity
// never touches the store at all.
func (p *GovernancePlugin) resolveComplexitySessionState(
	ctx *schemas.BifrostContext,
	req *schemas.BifrostRequest,
	virtualKey *configstoreTables.TableVirtualKey,
) *complexitySessionState {
	config := p.complexitySessionConfig.Load()
	if config == nil || p.complexitySessionStore.Load() == nil {
		return nil
	}

	sessionID, source, ok := resolveSessionID(ctx, req, config.IdentitySources)
	if !ok {
		return nil
	}

	// Sessions are namespaced per virtual key so one tenant's conversation can
	// never adopt another's pinned tier, even if both present the same id.
	tenantID := complexitySessionGlobalTenant
	if virtualKey != nil && virtualKey.ID != "" {
		tenantID = virtualKey.ID
	}
	key, ok := buildComplexitySessionKey(tenantID, sessionID)
	if !ok {
		return nil
	}

	ttl := config.TTL
	if ttl <= 0 {
		// Normalization fills this in, so reaching here means a record was
		// written by an older build or hand-edited. A non-positive ttl is
		// rejected by the store, which would disable sessions silently.
		return nil
	}

	state := &complexitySessionState{ID: sessionID, Source: source, Key: key, TTL: ttl}
	// Carried on the context because the response path cannot redo this: identity
	// resolution reads the request body, which is gone by then.
	ctx.SetValue(complexitySessionContextKey, state)
	return state
}

// recordSessionRouteObservation stores what the provider reported about cache
// reuse for the route that actually served this turn. Cache-aware switching
// decides whether discarding a warm cache is worth it, which is unanswerable
// without these facts.
//
// It records nothing on non-final chunks: usage generally only arrives with the
// last one, so recording earlier would persist zeros and read as "no cache worth
// keeping" on every streamed request.
func (p *GovernancePlugin) recordSessionRouteObservation(
	ctx *schemas.BifrostContext,
	result *schemas.BifrostResponse,
	provider schemas.ModelProvider,
	model string,
	isFinalChunk bool,
) {
	if !isFinalChunk || result == nil {
		return
	}
	state, _ := ctx.Value(complexitySessionContextKey).(*complexitySessionState)
	if state == nil {
		return
	}
	stored := p.complexitySessionStore.Load()
	if stored == nil {
		return
	}
	store := *stored

	routeID := effectiveSessionRouteIdentity(ctx, provider, model)
	if routeID == "" {
		return
	}
	cachedTokens, cacheObserved := sessionCachedReadTokens(result)

	// Read before writing. Update is unconditionally a write, and on a replicated
	// backend a write is a cluster message — recording an unchanged observation
	// every turn would undo the read-path work and cost one broadcast per request.
	record, found, err := store.Get(ctx, state.Key, state.TTL)
	if err != nil {
		if p.logger != nil {
			p.logger.Debug("[Governance] Could not read complexity session to record route observation: %v", err)
		}
		return
	}
	if !found {
		return
	}

	observedAt := time.Now()
	if !sessionObservationNeedsUpdate(record.RouteObservations[routeID], cachedTokens, cacheObserved, observedAt, state.TTL) {
		return
	}

	if _, _, err := store.Update(ctx, state.Key, state.TTL, func(current *SessionComplexityRecord) error {
		if current.RouteObservations == nil {
			current.RouteObservations = make(map[string]SessionRouteObservation, 1)
		}
		current.RouteObservations[routeID] = SessionRouteObservation{
			CachedReadTokens: cachedTokens,
			CacheObserved:    cacheObserved,
			LastSeenAt:       observedAt,
		}
		boundSessionRouteObservations(current.RouteObservations, maxSessionRouteObservations)
		return nil
	}); err != nil && p.logger != nil {
		p.logger.Debug("[Governance] Could not record complexity session route observation: %v", err)
	}
}

// sessionObservationNeedsUpdate decides whether this turn told us anything the
// stored observation does not already say.
func sessionObservationNeedsUpdate(
	previous SessionRouteObservation,
	cachedTokens int,
	cacheObserved bool,
	observedAt time.Time,
	ttl time.Duration,
) bool {
	// Never seen this route.
	if previous.LastSeenAt.IsZero() {
		return true
	}
	// Whether the provider reports cache telemetry at all is a different fact
	// from how much it reported, and it flips how the number should be read.
	if previous.CacheObserved != cacheObserved {
		return true
	}
	if cacheObserved && sessionCachedTokensMateriallyChanged(previous.CachedReadTokens, cachedTokens) {
		return true
	}
	// Otherwise refresh on the same cadence as the read path, so LastSeenAt stays
	// roughly current without a write per turn.
	return observedAt.Sub(previous.LastSeenAt) >= complexitySessionRefreshInterval(ttl)
}

// sessionCachedTokensMateriallyChanged treats any crossing of zero as material:
// that is the difference between "there is a cache to protect" and "there is
// not", which is the question cache-aware switching actually asks.
func sessionCachedTokensMateriallyChanged(previous, next int) bool {
	if previous == next {
		return false
	}
	if previous == 0 || next == 0 {
		return true
	}
	delta := next - previous
	if delta < 0 {
		delta = -delta
	}
	return float64(delta) > float64(previous)*sessionObservationChangeRatio
}

// boundSessionRouteObservations evicts the least recently seen routes until the
// map fits. Recency is the right axis: a route not seen for a while is not the
// one a switch would be giving up.
func boundSessionRouteObservations(observations map[string]SessionRouteObservation, limit int) {
	for len(observations) > limit {
		oldestKey := ""
		var oldestSeen time.Time
		for key, observation := range observations {
			if oldestKey == "" || observation.LastSeenAt.Before(oldestSeen) {
				oldestKey, oldestSeen = key, observation.LastSeenAt
			}
		}
		delete(observations, oldestKey)
	}
}

// effectiveSessionRouteIdentity identifies the route that actually served, which
// is not necessarily the one routing selected: a fallback changes provider and
// model, and key rotation changes which cache is warm. The value is hashed
// because callers only ever compare identities, and the record replicates.
func effectiveSessionRouteIdentity(ctx *schemas.BifrostContext, provider schemas.ModelProvider, model string) string {
	keyID := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeySelectedKeyID)
	if provider == "" && model == "" && keyID == "" {
		return ""
	}
	return complexitySessionHash(string(provider) + "\x00" + model + "\x00" + keyID)
}

// sessionCachedReadTokens reports the provider's cached-input count and whether
// it reported one at all. The second value matters: a provider that omits cache
// telemetry is not a provider reporting a cold cache, and collapsing the two
// would let a switch discard a warm cache on every provider that stays quiet.
// The chat and responses APIs carry the same fact under different types, so
// each is unwrapped on its own rather than through a shared usage value.
func sessionCachedReadTokens(result *schemas.BifrostResponse) (int, bool) {
	switch {
	case result.ChatResponse != nil && result.ChatResponse.Usage != nil:
		return chatCachedReadTokens(result.ChatResponse.Usage)
	case result.TextCompletionResponse != nil && result.TextCompletionResponse.Usage != nil:
		return chatCachedReadTokens(result.TextCompletionResponse.Usage)
	case result.ResponsesResponse != nil && result.ResponsesResponse.Usage != nil:
		return responsesCachedReadTokens(result.ResponsesResponse.Usage)
	case result.ResponsesStreamResponse != nil &&
		result.ResponsesStreamResponse.Response != nil &&
		result.ResponsesStreamResponse.Response.Usage != nil:
		return responsesCachedReadTokens(result.ResponsesStreamResponse.Response.Usage)
	}
	return 0, false
}

func chatCachedReadTokens(usage *schemas.BifrostLLMUsage) (int, bool) {
	if usage.PromptTokensDetails == nil {
		return 0, false
	}
	return usage.PromptTokensDetails.CachedReadTokens, true
}

func responsesCachedReadTokens(usage *schemas.ResponsesResponseUsage) (int, bool) {
	if usage.InputTokensDetails == nil {
		return 0, false
	}
	return usage.InputTokensDetails.CachedReadTokens, true
}

// publishSessionKeyAffinity hands the resolved conversation identity to the
// core's API-key stickiness.
//
// Without this the feature protects the wrong layer. A provider's prompt cache
// is keyed per API key, so holding a conversation on one tier while its key
// rotates between turns discards the cache anyway — the pin would do the
// bookkeeping and deliver none of the benefit.
//
// It is safe to publish unconditionally here because state is only non-nil when
// session behaviour is switched on and something identified the conversation.
// That matters: core activates stickiness on any non-empty session id, so
// writing a derived id outside those conditions would silently pin API keys for
// callers who never asked for it.
func (p *GovernancePlugin) publishSessionKeyAffinity(ctx *schemas.BifrostContext, state *complexitySessionState) {
	if state == nil {
		return
	}
	// A header-sourced id is already here and this rewrites the same value; a
	// harness- or fingerprint-derived one is new, which is the whole point.
	ctx.SetValue(schemas.BifrostContextKeySessionID, state.ID)

	// Align the two lifetimes. Core otherwise falls back to its own one-hour
	// default, so a shorter configured session would release its tier while the
	// key stayed pinned — or a longer one would keep classifying against a key
	// that had already rotated. An explicit per-request ttl still wins: the
	// caller asked for it by name.
	if existing, ok := ctx.Value(schemas.BifrostContextKeySessionTTL).(time.Duration); !ok || existing <= 0 {
		ctx.SetValue(schemas.BifrostContextKeySessionTTL, state.TTL)
	}
}

// withComplexitySession wraps the lazy classification closure so an established
// session reuses its tier instead of classifying again. That reuse is the point
// of the feature: it keeps the provider's prompt cache warm and skips the
// per-turn embedding call.
//
// It deliberately wraps rather than running ahead of classification. The inner
// closure is only invoked when a routing rule references complexity_tier, so
// wrapping inherits that condition for free and a request that never routes on
// complexity performs no session reads.
func (p *GovernancePlugin) withComplexitySession(
	ctx *schemas.BifrostContext,
	state *complexitySessionState,
	classify func() *complexity.ComplexityResult,
) func() *complexity.ComplexityResult {
	if state == nil || classify == nil {
		return classify
	}
	stored := p.complexitySessionStore.Load()
	if stored == nil {
		return classify
	}
	store := *stored

	return func() *complexity.ComplexityResult {
		// A store that cannot be read is not a reason to fail the request: fall
		// through and classify, which is exactly the pre-session behaviour.
		record, found, err := store.Get(ctx, state.Key, state.TTL)
		if err != nil {
			if p.logger != nil {
				p.logger.Warn("[Governance] Complexity session lookup failed, classifying this turn: %v", err)
			}
		} else if found && record != nil && record.Tier != "" {
			p.publishSessionTier(ctx, state, record)
			return &complexity.ComplexityResult{Tier: record.Tier}
		}

		result := classify()
		// Only a real tier is worth remembering. Persisting a failed or rejected
		// classification would pin the session to "no tier" for the whole ttl,
		// turning one bad turn into a session-long outage of complexity routing.
		if result == nil || result.Tier == "" {
			return result
		}

		// RuleID is deliberately left unset. The tier is classifier memory about
		// the conversation, not state owned by whichever rule happened to consult
		// it first — binding them would let an unrelated rule edit drop every
		// live session's tier.
		created, err := store.Create(ctx, state.Key, &SessionComplexityRecord{
			Tier:      result.Tier,
			DecidedAt: time.Now(),
		}, state.TTL)
		if err != nil && p.logger != nil {
			p.logger.Warn("[Governance] Failed to persist complexity session tier, the next turn will classify again: %v", err)
		}
		if created {
			ctx.AppendRoutingEngineLog(
				schemas.RoutingEngineRoutingRule,
				schemas.LogLevelInfo,
				"Complexity session established: tier="+result.Tier+" identity="+state.Source,
			)
		}
		return result
	}
}

// publishSessionTier records a held tier the same way a fresh classification is
// recorded, so downstream logging sees a tier rather than a gap — with a
// mechanism that says it was reused, not embedded.
func (p *GovernancePlugin) publishSessionTier(
	ctx *schemas.BifrostContext,
	state *complexitySessionState,
	record *SessionComplexityRecord,
) {
	ctx.SetValue(schemas.BifrostContextKeyGovernanceComplexityTier, record.Tier)
	ctx.SetValue(schemas.BifrostContextKeyGovernanceComplexityMechanism, complexity.MechanismSession)
	if p.logger != nil {
		p.logger.Debug("[Governance] Complexity session hit: tier=%s identity=%s", record.Tier, state.Source)
	}
	ctx.AppendRoutingEngineLog(
		schemas.RoutingEngineRoutingRule,
		schemas.LogLevelInfo,
		"Complexity tier held from session: tier="+record.Tier+" identity="+state.Source+
			" (no classification ran for this turn)",
	)
}
