package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Signing constants for the Huawei Cloud SDK-HMAC-SHA256 request signature.
//
// The official CodeArts Agent extension signs its upstream calls with this
// algorithm:
//
//	canonicalRequest = method \n canonicalURI \n canonicalQuery \n
//	                   canonicalHeaders \n signedHeaders \n payloadHash
//	stringToSign     = "SDK-HMAC-SHA256" \n xSdkDate \n sha256hex(canonicalRequest)
//	signature        = hmacSHA256(secretKey, stringToSign)
//	Authorization    = SDK-HMAC-SHA256 Access=<AK>, SignedHeaders=<...>, Signature=<...>
const (
	signingAlgorithm  = "SDK-HMAC-SHA256"
	headerXSDKDate    = "X-Sdk-Date"
	headerAuth        = "Authorization"
	headerContentSHA  = "X-Sdk-Content-Sha256"
	headerSecurityTok = "X-Security-Token"
	headerDomainID    = "X-Domain-Id"

	emptyBodySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	// sdkDateFormat is the UTC timestamp layout used by X-Sdk-Date.
	sdkDateFormat = "20060102T150405Z"
)

// credential is a resolved upstream credential: a temporary AK/SK pair plus the
// security token and domain returned by the login flow.
type credential struct {
	AccessKeyID     string             `json:"access_key_id"`
	SecretAccessKey string             `json:"secret_access_key"`
	SecurityToken   string             `json:"security_token"`
	DomainID        string             `json:"domain_id"`
	UserName        string             `json:"user_name"`
	UserID          string             `json:"user_id"`
	ExpiresAt       string             `json:"expires_at"`
	LoginType       string             `json:"login_type"`
	RefreshToken    string             `json:"refresh_token,omitempty"`
	OAuthContext    *oauthLoginContext `json:"oauth_context,omitempty"`
}

func (c *credential) valid() bool {
	return c != nil && c.AccessKeyID != "" && c.SecretAccessKey != ""
}

// signRequest signs an upstream request in place by adding X-Sdk-Date (when it
// is not already present) and Authorization, and returns the headers map that
// must be sent upstream.
//
// The canonical header set is derived from the supplied headers exactly like
// the client implementation: header names are lower-cased, de-duplicated in
// iteration order, sorted lexicographically and joined with ";". Every value is
// trimmed before it enters the canonical request.
func signRequest(method, rawURL string, headers map[string]string, body []byte, cred *credential, includeHost bool) (map[string]string, error) {
	if !cred.valid() {
		return nil, fmt.Errorf("credential is incomplete: access key and secret key are both required")
	}
	parsed, errParse := url.Parse(rawURL)
	if errParse != nil {
		return nil, fmt.Errorf("parse upstream url: %w", errParse)
	}

	out := make(map[string]string, len(headers)+3)
	for key, value := range headers {
		out[key] = value
	}
	if cred.SecurityToken != "" {
		out[headerSecurityTok] = cred.SecurityToken
	}
	if cred.DomainID != "" {
		out[headerDomainID] = cred.DomainID
	}

	if !hasHeaderFold(out, headerXSDKDate) {
		out[headerXSDKDate] = time.Now().UTC().Format(sdkDateFormat)
	}
	if includeHost {
		if _, ok := lookHeaderFold(out, "host"); !ok {
			out["Host"] = parsed.Host
		}
	}
	// The host passes this map directly to net/http. Its serializer excludes
	// only the canonical "Host" key when emitting the URL's Host header; a
	// lower-case key would be written as a second Host and rejected with 400.
	if host, ok := lookHeaderFold(out, "host"); ok {
		for name := range out {
			if strings.EqualFold(name, "host") {
				delete(out, name)
			}
		}
		out["Host"] = host
	}

	// Lower-case canonical view; later duplicates win, matching the reference
	// implementation which builds the map by iterating the original key order.
	canonical := make(map[string]string, len(out))
	for key, value := range out {
		canonical[strings.ToLower(key)] = value
	}

	names := make([]string, 0, len(canonical))
	for name := range canonical {
		names = append(names, name)
	}
	sort.Strings(names)

	var headerBuilder strings.Builder
	for _, name := range names {
		headerBuilder.WriteString(name)
		headerBuilder.WriteString(":")
		headerBuilder.WriteString(strings.TrimSpace(canonical[name]))
		headerBuilder.WriteString("\n")
	}
	signedHeaders := strings.Join(names, ";")

	payloadHash := sha256Hex(body)
	if override, ok := canonical[strings.ToLower(headerContentSHA)]; ok && strings.TrimSpace(override) != "" {
		payloadHash = strings.TrimSpace(override)
	} else if !requestCarriesBody(method) {
		payloadHash = emptyBodySHA256
	}

	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI(parsed.Path),
		canonicalQueryString(parsed.Query()),
		headerBuilder.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	stringToSign := strings.Join([]string{
		signingAlgorithm,
		out[headerXSDKDate],
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signature := hmacSHA256Hex([]byte(cred.SecretAccessKey), stringToSign)
	out[headerAuth] = fmt.Sprintf("%s Access=%s, SignedHeaders=%s, Signature=%s",
		signingAlgorithm, cred.AccessKeyID, signedHeaders, signature)

	return out, nil
}

// requestCarriesBody reports whether the HTTP method is expected to carry a
// body for signing purposes. Only PUT, PATCH and POST are treated as body
// bearing; everything else signs the empty-body digest.
func requestCarriesBody(method string) bool {
	switch strings.ToUpper(method) {
	case "PUT", "PATCH", "POST":
		return true
	default:
		return false
	}
}

// canonicalURI escapes every path segment using the RFC 3986 unreserved set and
// guarantees a trailing slash, matching the reference signer.
func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	segments := strings.Split(path, "/")
	for index, segment := range segments {
		segments[index] = encodeRFC3986(segment)
	}
	escaped := strings.Join(segments, "/")
	if !strings.HasSuffix(escaped, "/") {
		escaped += "/"
	}
	return escaped
}

// canonicalQueryString sorts query keys, sorts repeated values and escapes both
// sides with the RFC 3986 unreserved set.
func canonicalQueryString(values url.Values) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		items := append([]string(nil), values[key]...)
		sort.Strings(items)
		for _, item := range items {
			parts = append(parts, encodeRFC3986(key)+"="+encodeRFC3986(item))
		}
	}
	return strings.Join(parts, "&")
}

// unreserved reports whether b is an RFC 3986 unreserved character. This is the
// exact character set left untouched by JavaScript encodeURIComponent, which
// the reference signer uses.
func unreserved(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z',
		b >= 'a' && b <= 'z',
		b >= '0' && b <= '9':
		return true
	}
	switch b {
	case '-', '_', '.', '!', '~', '*', '\'', '(', ')':
		return true
	}
	return false
}

// encodeRFC3986 percent-encodes all but the unreserved set, emitting upper-case
// hex digits.
func encodeRFC3986(value string) string {
	const hexDigits = "0123456789ABCDEF"
	var builder strings.Builder
	for i := 0; i < len(value); i++ {
		b := value[i]
		if unreserved(b) {
			builder.WriteByte(b)
			continue
		}
		builder.WriteByte('%')
		builder.WriteByte(hexDigits[b>>4])
		builder.WriteByte(hexDigits[b&0x0f])
	}
	return builder.String()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256Hex(key []byte, data string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}

func hasHeaderFold(headers map[string]string, name string) bool {
	_, ok := lookHeaderFold(headers, name)
	return ok
}

func lookHeaderFold(headers map[string]string, name string) (string, bool) {
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return value, true
		}
	}
	return "", false
}
