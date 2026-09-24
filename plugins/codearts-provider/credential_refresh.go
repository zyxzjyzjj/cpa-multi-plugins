package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const onDemandRefreshLead = 15 * time.Minute

// credentialRefreshLocks serializes refresh-token rotation per account. OAuth
// refresh tokens can rotate, so two requests must not exchange the same token
// concurrently after an idle period.
var credentialRefreshLocks sync.Map

// refreshedCredentialSnapshots bridges the small window between a plugin
// refresh returning and CPA writing the refreshed auth record back to storage.
var refreshedCredentialSnapshots sync.Map

func credentialNeedsRefresh(cred *credential, lead time.Duration) bool {
	if cred == nil || strings.TrimSpace(cred.SecurityToken) == "" {
		return false
	}
	expiresAt := strings.TrimSpace(cred.ExpiresAt)
	if expiresAt == "" {
		return false
	}
	expiry, errParse := time.Parse(time.RFC3339, expiresAt)
	if errParse != nil {
		// A malformed expiry cannot safely be treated as fresh. A successful
		// renewal replaces it with the upstream's canonical timestamp.
		return true
	}
	return !expiry.After(time.Now().Add(lead))
}

func credentialRefreshKey(cred *credential) string {
	if cred == nil {
		return providerID
	}
	identity := strings.TrimSpace(cred.DomainID) + "\x00" + strings.TrimSpace(cred.UserID)
	if identity == "\x00" {
		identity = strings.TrimSpace(cred.UserName)
	}
	if identity == "" {
		identity = strings.TrimSpace(cred.AccessKeyID)
	}
	return sha256Hex([]byte(providerID + "\x00" + identity))
}

func sameCredentialAccount(left, right *credential) bool {
	if left == nil || right == nil {
		return false
	}
	if left.DomainID != "" && left.UserID != "" && right.DomainID != "" && right.UserID != "" {
		return left.DomainID == right.DomainID && left.UserID == right.UserID
	}
	if left.UserName != "" && right.UserName != "" {
		return left.UserName == right.UserName
	}
	return left.AccessKeyID != "" && left.AccessKeyID == right.AccessKeyID
}

func credentialMaterialChanged(left, right *credential) bool {
	if left == nil || right == nil {
		return left != right
	}
	return left.AccessKeyID != right.AccessKeyID ||
		left.SecretAccessKey != right.SecretAccessKey ||
		left.SecurityToken != right.SecurityToken ||
		left.RefreshToken != right.RefreshToken ||
		left.ExpiresAt != right.ExpiresAt
}

func credentialExpiry(cred *credential) time.Time {
	if cred == nil {
		return time.Time{}
	}
	parsed, _ := time.Parse(time.RFC3339, strings.TrimSpace(cred.ExpiresAt))
	return parsed
}

func rememberRefreshedCredential(cred *credential) {
	if cred == nil || !cred.valid() {
		return
	}
	copyCredential := *cred
	refreshedCredentialSnapshots.Store(credentialRefreshKey(cred), &copyCredential)
}

func newerCredentialSnapshot(fallback *credential) *credential {
	if fallback == nil {
		return nil
	}
	value, ok := refreshedCredentialSnapshots.Load(credentialRefreshKey(fallback))
	if !ok {
		return nil
	}
	snapshot, _ := value.(*credential)
	if snapshot == nil || !sameCredentialAccount(snapshot, fallback) {
		return nil
	}
	snapshotExpiry := credentialExpiry(snapshot)
	fallbackExpiry := credentialExpiry(fallback)
	if !snapshotExpiry.IsZero() && (fallbackExpiry.IsZero() || snapshotExpiry.After(fallbackExpiry)) {
		copyCredential := *snapshot
		return &copyCredential
	}
	return nil
}

// latestStoredCredential reloads the account after acquiring its refresh lock.
// This prevents a request that was queued behind another refresh from reusing a
// rotated refresh token contained in its stale ExecutorRequest.StorageJSON.
func latestStoredCredential(authID string, fallback *credential) *credential {
	if snapshot := newerCredentialSnapshot(fallback); snapshot != nil {
		return snapshot
	}
	files, errList := hostAuthList()
	if errList != nil {
		return fallback
	}
	authID = strings.TrimSpace(authID)
	for _, file := range files {
		if normalizeProvider(file.Provider) != providerID && normalizeProvider(file.Type) != providerID {
			continue
		}
		storage, errGet := hostAuthGet(file.AuthIndex)
		if errGet != nil {
			continue
		}
		candidate, errCredential := credentialFromStorage(storage)
		if errCredential != nil || !candidate.valid() {
			continue
		}
		matchesID := authID != "" && (file.AuthIndex == authID || file.ID == authID || file.Name == authID)
		if matchesID || sameCredentialAccount(candidate, fallback) {
			return candidate
		}
	}
	return fallback
}

func credentialFromRefreshEnvelope(raw []byte) (*credential, error) {
	var rpc envelope
	if errDecode := json.Unmarshal(raw, &rpc); errDecode != nil {
		return nil, fmt.Errorf("decode credential refresh response: %w", errDecode)
	}
	if !rpc.OK {
		if rpc.Error != nil && strings.TrimSpace(rpc.Error.Message) != "" {
			return nil, fmt.Errorf("%s", rpc.Error.Message)
		}
		return nil, fmt.Errorf("credential refresh was rejected")
	}
	var response pluginapi.AuthRefreshResponse
	if errDecode := json.Unmarshal(rpc.Result, &response); errDecode != nil {
		return nil, fmt.Errorf("decode refreshed credential: %w", errDecode)
	}
	updated, errCredential := credentialFromStorage(response.Auth.StorageJSON)
	if errCredential != nil || !updated.valid() {
		return nil, fmt.Errorf("credential refresh returned incomplete storage")
	}
	return updated, nil
}

// refreshCredentialViaProvider reuses the auth-provider implementation so the
// host-driven, cron-driven, and request-driven paths cannot drift apart.
func refreshCredentialViaProvider(authID string, cred *credential) (*credential, error) {
	if cred == nil || !cred.valid() {
		return nil, fmt.Errorf("stored credential is incomplete")
	}
	storage, errStorage := json.Marshal(map[string]any{storageKey: *cred})
	if errStorage != nil {
		return nil, errStorage
	}
	request, errRequest := json.Marshal(pluginapi.AuthRefreshRequest{AuthID: authID, StorageJSON: storage})
	if errRequest != nil {
		return nil, errRequest
	}
	raw, errRefresh := authRefresh(request)
	if errRefresh != nil {
		return nil, errRefresh
	}
	return credentialFromRefreshEnvelope(raw)
}

// ensureFreshCredential performs a silent refresh before a model lookup or chat
// uses an expired (or nearly expired) temporary credential. It is the catch-up
// path for a sleeping/restarted CPA that missed the normal hourly timer.
func ensureFreshCredential(authID string, cred *credential) (*credential, error) {
	if !credentialNeedsRefresh(cred, onDemandRefreshLead) {
		return cred, nil
	}
	updated, errRefresh := refreshCredentialViaProvider(authID, cred)
	if errRefresh != nil {
		return cred, errRefresh
	}
	if errPersist := persistRenewedCredential(cred, updated); errPersist != nil {
		// The refreshed value is still safe for this in-flight request. Report the
		// persistence failure so it is visible, while avoiding an unnecessary
		// upstream outage for the current caller.
		logWarn("refreshed credential could not be persisted; using it for the current request", map[string]any{
			"error": errPersist.Error(),
		})
	}
	return updated, nil
}

func prepareCredentialForUse(authID string, cred *credential) (*credential, error) {
	updated, errRefresh := ensureFreshCredential(authID, cred)
	if errRefresh == nil {
		return updated, nil
	}
	// If the old token still has time left, keep serving with it and let the
	// periodic paths retry. Once it is expired, fail before sending a request the
	// gateway may misleadingly report as an IAM 503.
	if !credentialNeedsRefresh(cred, 0) {
		logWarn("proactive credential refresh failed; retaining the unexpired credential", map[string]any{
			"error": errRefresh.Error(),
		})
		return cred, nil
	}
	return cred, errRefresh
}
