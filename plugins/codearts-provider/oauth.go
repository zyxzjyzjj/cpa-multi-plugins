package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	codeArtsOAuthClientID    = "vscode-codebot"
	codeArtsOAuthURIScheme   = "vscode-codebot"
	codeArtsOAuthTokenURL    = "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens"
	codeArtsOAuthIdentityURL = "https://sts.cn-north-4.myhuaweicloud.com/v5/caller-identity"
	codeArtsOAuthCallback    = "/oauth/callback"
	codeArtsOAuthPKCEMethod  = "SHA-256"
	codeArtsOAuthDPoPAlg     = "ES256"
)

// oauthLoginContext is the long-lived half of the new CodeArts login. The
// official 26.9.x extension persists both the PKCE verifier and the DPoP key
// pair because the same proof key is required when the refresh token is used.
type oauthLoginContext struct {
	PKCEPair    oauthPKCEPair    `json:"pkce_pair"`
	DPoPKeyPair oauthDPoPKeyPair `json:"dpop_key_pair"`
}

type oauthPKCEPair struct {
	CodeVerifier        string `json:"code_verifier"`
	CodeChallenge       string `json:"code_challenge"`
	CodeChallengeMethod string `json:"code_challenge_method"`
}

type oauthDPoPKeyPair struct {
	PrivateKeyJWK oauthJWK `json:"private_key_jwk"`
	PublicKeyJWK  oauthJWK `json:"public_key_jwk"`
}

type oauthJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	D   string `json:"d,omitempty"`
}

type oauthTokenResponse struct {
	Credentials struct {
		AccessKeyID     string `json:"access_key_id"`
		Expiration      string `json:"expiration"`
		SecretAccessKey string `json:"secret_access_key"`
		SecurityToken   string `json:"security_token"`
	} `json:"credentials"`
	RefreshToken string `json:"refresh_token"`
}

type oauthIdentity struct {
	AccountID    string `json:"account_id"`
	PrincipalID  string `json:"principal_id"`
	PrincipalURN string `json:"principal_urn"`
}

func newOAuthLoginContext() (*oauthLoginContext, error) {
	key, errKey := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if errKey != nil {
		return nil, fmt.Errorf("generate DPoP key: %w", errKey)
	}
	verifierBytes := make([]byte, 64)
	if _, errRead := rand.Read(verifierBytes); errRead != nil {
		return nil, fmt.Errorf("generate PKCE verifier: %w", errRead)
	}
	verifier := fmt.Sprintf("%x", verifierBytes)
	challengeHash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeHash[:])

	public := oauthJWK{
		Kty: "EC",
		Crv: "P-256",
		X:   base64.RawURLEncoding.EncodeToString(paddedScalar(key.X, 32)),
		Y:   base64.RawURLEncoding.EncodeToString(paddedScalar(key.Y, 32)),
	}
	private := public
	private.D = base64.RawURLEncoding.EncodeToString(paddedScalar(key.D, 32))
	return &oauthLoginContext{
		PKCEPair: oauthPKCEPair{
			CodeVerifier:        verifier,
			CodeChallenge:       challenge,
			CodeChallengeMethod: codeArtsOAuthPKCEMethod,
		},
		DPoPKeyPair: oauthDPoPKeyPair{PrivateKeyJWK: private, PublicKeyJWK: public},
	}, nil
}

func paddedScalar(value *big.Int, size int) []byte {
	out := make([]byte, size)
	if value == nil {
		return out
	}
	raw := value.Bytes()
	if len(raw) > size {
		raw = raw[len(raw)-size:]
	}
	copy(out[size-len(raw):], raw)
	return out
}

func oauthPrivateKey(context *oauthLoginContext) (*ecdsa.PrivateKey, error) {
	if context == nil {
		return nil, fmt.Errorf("OAuth login context is missing")
	}
	jwk := context.DPoPKeyPair.PrivateKeyJWK
	if jwk.Kty != "EC" || jwk.Crv != "P-256" || jwk.D == "" {
		return nil, fmt.Errorf("OAuth DPoP private key is incomplete")
	}
	d, errDecode := base64.RawURLEncoding.DecodeString(jwk.D)
	if errDecode != nil || len(d) == 0 {
		return nil, fmt.Errorf("decode OAuth DPoP private key")
	}
	curve := elliptic.P256()
	x, y := curve.ScalarBaseMult(d)
	if x == nil || y == nil {
		return nil, fmt.Errorf("OAuth DPoP private key is invalid")
	}
	return &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y}, D: new(big.Int).SetBytes(d)}, nil
}

func dpopProof(context *oauthLoginContext, method, endpoint string) (string, error) {
	key, errKey := oauthPrivateKey(context)
	if errKey != nil {
		return "", errKey
	}
	public := oauthJWK{
		Kty: "EC",
		Crv: "P-256",
		X:   base64.RawURLEncoding.EncodeToString(paddedScalar(key.X, 32)),
		Y:   base64.RawURLEncoding.EncodeToString(paddedScalar(key.Y, 32)),
	}
	header, _ := json.Marshal(map[string]any{"alg": codeArtsOAuthDPoPAlg, "typ": "dpop+jwt", "jwk": public})
	payload, _ := json.Marshal(map[string]any{
		"htm": strings.ToUpper(method),
		"htu": endpoint,
		"iat": time.Now().Unix(),
		"jti": randomHex(32),
	})
	encodedHeader := base64.RawURLEncoding.EncodeToString(header)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	input := encodedHeader + "." + encodedPayload
	digest := sha256.Sum256([]byte(input))
	r, s, errSign := ecdsa.Sign(rand.Reader, key, digest[:])
	if errSign != nil {
		return "", fmt.Errorf("sign DPoP proof: %w", errSign)
	}
	signature := append(paddedScalar(r, 32), paddedScalar(s, 32)...)
	return input + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func oauthExchangeAuthorizationCode(cfg *Config, code, redirectURI string, context *oauthLoginContext) (*credential, int, string, error) {
	if context == nil || strings.TrimSpace(context.PKCEPair.CodeVerifier) == "" {
		return nil, 0, "", fmt.Errorf("OAuth PKCE context is missing")
	}
	form := url.Values{
		"client_id":     {codeArtsOAuthClientID},
		"code":          {strings.TrimSpace(code)},
		"code_verifier": {context.PKCEPair.CodeVerifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	}
	return oauthTokenExchange(cfg, context, form, "")
}

func oauthRefreshCredential(cfg *Config, existing *credential) (*credential, int, string, error) {
	if existing == nil || existing.OAuthContext == nil || strings.TrimSpace(existing.RefreshToken) == "" {
		return nil, 0, "", fmt.Errorf("stored OAuth refresh context is incomplete")
	}
	form := url.Values{
		"client_id":     {codeArtsOAuthClientID},
		"code_verifier": {existing.OAuthContext.PKCEPair.CodeVerifier},
		"grant_type":    {"refresh_token"},
		"refresh_token": {existing.RefreshToken},
	}
	updated, status, message, errExchange := oauthTokenExchange(cfg, existing.OAuthContext, form, existing.RefreshToken)
	if updated != nil {
		updated.DomainID = firstNonEmptyString(updated.DomainID, existing.DomainID)
		updated.UserID = firstNonEmptyString(updated.UserID, existing.UserID)
		updated.UserName = firstNonEmptyString(updated.UserName, existing.UserName)
		updated.LoginType = firstNonEmptyString(updated.LoginType, existing.LoginType, "WEB")
	}
	return updated, status, message, errExchange
}

func oauthTokenExchange(cfg *Config, context *oauthLoginContext, form url.Values, previousRefreshToken string) (*credential, int, string, error) {
	if context == nil || strings.TrimSpace(context.PKCEPair.CodeVerifier) == "" {
		return nil, 0, "", fmt.Errorf("OAuth PKCE context is missing")
	}
	endpoint := firstNonEmptyString(cfg.OAuthTokenURL, codeArtsOAuthTokenURL)
	proof, errProof := dpopProof(context, http.MethodPost, endpoint)
	if errProof != nil {
		return nil, 0, "", errProof
	}
	response, errDo := hostHTTPDo(http.MethodPost, endpoint, map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/x-www-form-urlencoded",
		"DPoP":         proof,
	}, []byte(form.Encode()))
	if errDo != nil {
		return nil, 0, truncate(errDo.Error(), 300), fmt.Errorf("OAuth token request failed: %w", errDo)
	}
	if response.StatusCode != http.StatusOK {
		message := truncate(string(response.Body), 500)
		return nil, response.StatusCode, message, fmt.Errorf("OAuth token request returned HTTP %d: %s", response.StatusCode, message)
	}
	var token oauthTokenResponse
	if errDecode := json.Unmarshal(response.Body, &token); errDecode != nil {
		return nil, response.StatusCode, truncate(errDecode.Error(), 300), fmt.Errorf("decode OAuth token response: %w", errDecode)
	}
	cred := &credential{
		AccessKeyID:     strings.TrimSpace(token.Credentials.AccessKeyID),
		SecretAccessKey: strings.TrimSpace(token.Credentials.SecretAccessKey),
		SecurityToken:   strings.TrimSpace(token.Credentials.SecurityToken),
		ExpiresAt:       strings.TrimSpace(token.Credentials.Expiration),
		RefreshToken:    firstNonEmptyString(token.RefreshToken, previousRefreshToken),
		OAuthContext:    context,
		LoginType:       "WEB",
	}
	if !cred.valid() || cred.SecurityToken == "" || cred.RefreshToken == "" {
		return nil, response.StatusCode, "OAuth response contains incomplete credentials", fmt.Errorf("OAuth token response is incomplete")
	}
	identity, errIdentity := identityFromRefreshToken(cred.RefreshToken)
	if errIdentity != nil || !identity.valid() {
		identity, errIdentity = fetchOAuthIdentity(cfg, cred)
	}
	if errIdentity != nil {
		return nil, response.StatusCode, truncate(errIdentity.Error(), 300), errIdentity
	}
	applyOAuthIdentity(cred, identity)
	return cred, response.StatusCode, "", nil
}

func (identity oauthIdentity) valid() bool {
	return identity.AccountID != "" && identity.PrincipalID != "" && identity.PrincipalURN != ""
}

func identityFromRefreshToken(refreshToken string) (oauthIdentity, error) {
	parts := strings.Split(refreshToken, ".")
	if len(parts) < 2 {
		return oauthIdentity{}, fmt.Errorf("refresh token is not a JWT")
	}
	payload, errDecode := decodeBase64URL(parts[1])
	if errDecode != nil {
		return oauthIdentity{}, fmt.Errorf("decode refresh token payload: %w", errDecode)
	}
	var claims struct {
		UserProfile string `json:"user_profile"`
	}
	if errJSON := json.Unmarshal(payload, &claims); errJSON != nil || claims.UserProfile == "" {
		return oauthIdentity{}, fmt.Errorf("refresh token contains no user_profile")
	}
	profile, errProfile := decodeBase64URL(claims.UserProfile)
	if errProfile != nil {
		return oauthIdentity{}, fmt.Errorf("decode refresh token user_profile: %w", errProfile)
	}
	var identity oauthIdentity
	if errJSON := json.Unmarshal(profile, &identity); errJSON != nil {
		return oauthIdentity{}, fmt.Errorf("decode refresh token identity: %w", errJSON)
	}
	if !identity.valid() {
		return oauthIdentity{}, fmt.Errorf("refresh token identity is incomplete")
	}
	return identity, nil
}

func decodeBase64URL(value string) ([]byte, error) {
	if decoded, errDecode := base64.RawURLEncoding.DecodeString(value); errDecode == nil {
		return decoded, nil
	}
	return base64.URLEncoding.DecodeString(value)
}

func fetchOAuthIdentity(cfg *Config, cred *credential) (oauthIdentity, error) {
	endpoint := firstNonEmptyString(cfg.OAuthIdentityURL, codeArtsOAuthIdentityURL)
	headers, errSign := signRequest(http.MethodGet, endpoint, map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/json",
	}, nil, cred, cfg.SignHost)
	if errSign != nil {
		return oauthIdentity{}, fmt.Errorf("sign OAuth identity request: %w", errSign)
	}
	response, errDo := hostHTTPDo(http.MethodGet, endpoint, headers, nil)
	if errDo != nil {
		return oauthIdentity{}, fmt.Errorf("OAuth identity request failed: %w", errDo)
	}
	if response.StatusCode != http.StatusOK {
		return oauthIdentity{}, fmt.Errorf("OAuth identity request returned HTTP %d: %s", response.StatusCode, truncate(string(response.Body), 300))
	}
	var identity oauthIdentity
	if errDecode := json.Unmarshal(response.Body, &identity); errDecode != nil {
		return oauthIdentity{}, fmt.Errorf("decode OAuth identity response: %w", errDecode)
	}
	if !identity.valid() {
		return oauthIdentity{}, fmt.Errorf("OAuth identity response is incomplete")
	}
	return identity, nil
}

func applyOAuthIdentity(cred *credential, identity oauthIdentity) {
	if cred == nil {
		return
	}
	cred.DomainID = strings.TrimSpace(identity.AccountID)
	cred.UserID = strings.TrimSpace(identity.PrincipalID)
	name := strings.TrimSpace(identity.PrincipalURN)
	if index := strings.LastIndex(name, ":"); index >= 0 && index+1 < len(name) {
		name = name[index+1:]
	}
	cred.UserName = name
}
