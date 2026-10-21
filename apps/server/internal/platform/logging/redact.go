package logging

import (
	"log/slog"
	"strings"
)

// Redacted is what replaces a value the logger refuses to emit.
const Redacted = "[REDACTED]"

// Redaction happens on two independent axes, because either one alone has a
// blind spot:
//
//   By NAME  — an attribute called "password" is a secret whatever it holds.
//              Blind spot: someone logs a credential under an innocuous key
//              such as "value", "arg", or "line".
//
//   By SHAPE — a value that looks like a PEM block, a bearer token, a JWT, an
//              EdgePilot enrollment token, or a DSN with an embedded password
//              is a secret whatever it is called. Blind spot: opaque secrets
//              with no recognisable shape, such as a raw random string.
//
// Together they cover a good deal more than either does alone. Neither is the
// primary control: config.Secret refuses to render itself at all, and that is
// what should actually be relied on. This package is the net under it, for the
// values that never passed through a Secret.

// sensitiveNames are attribute names that are always redacted, matched after
// normalisation (lowercased, separators removed).
var sensitiveNames = map[string]struct{}{
	"authorization":      {},
	"proxyauthorization": {},
	"cookie":             {},
	"setcookie":          {},
	"csr":                {},
	"jwt":                {},
	"bearer":             {},
	"pw":                 {},
	"pwd":                {},
	"otp":                {},
	"totp":               {},
	"pin":                {},
	"mfacode":            {},
	"mfasecret":          {},
	"recoverycode":       {},
	"datakey":            {},
	"encryptionkey":      {},
	"signingkey":         {},
	"sessionkey":         {},
	"accountkey":         {},
	"dsn":                {},
	"connectionstring":   {},
	"databaseurl":        {},
	"redisurl":           {},
	"sasurl":             {},
	"uploadurl":          {},
	"downloadurl":        {},
	"transferurl":        {},
	"signedurl":          {},
}

// sensitiveSuffixes redact any attribute whose normalised name ends with one of
// them.
//
// Suffix matching rather than substring matching is deliberate: "token_prefix",
// "token_count", "access_token_ttl", and "password_changed_at" are all safe and
// useful to log, and a substring rule would blank every one of them. Over-
// redaction is not free — it trains people to route around the logger.
//
// Note the absence of a bare "key": "idempotency_key", "object_key",
// "cache_key", and "ssh_key" (a public key) are all legitimate. The specific
// key names that ARE secret are listed in sensitiveNames above.
var sensitiveSuffixes = []string{
	"password",
	"passwd",
	"passphrase",
	"secret",
	"token",
	"credential",
	"credentials",
	"apikey",
	"privatekey",
	"authtoken",
}

// minSensitiveValueLen gates the shape rules. Every pattern below is longer
// than this, so most attributes are rejected on a single length comparison
// before any string scanning happens. It also removes a class of false
// positive: `token_prefix` holds a short, deliberately non-secret fragment such
// as "ep_enroll_9f3a", which shares a prefix with the real token but is safe
// and useful to log.
const minSensitiveValueLen = 16

// valuePrefixes catch secrets logged under an innocuous name.
var valuePrefixes = []string{
	"-----BEGIN",  // PEM: private keys, certificates, CSRs
	"Bearer ",     // an Authorization header logged as a bare string
	"Basic ",      //
	"ep_enroll_",  // EdgePilot enrollment token
	"ep_refresh_", // EdgePilot refresh token
	"xoxb-",       // Slack bot token, plausible in a webhook configuration
	"SG.",         // SendGrid API key
}

// normalizeKey lowercases and strips separators so that "Refresh-Token",
// "refresh_token", and "refreshToken" all reduce to the same thing.
func normalizeKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r == '_' || r == '-' || r == '.' || r == ' ':
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// IsSensitiveKey reports whether an attribute name should never be logged.
func IsSensitiveKey(key string) bool {
	n := normalizeKey(key)
	if _, ok := sensitiveNames[n]; ok {
		return true
	}
	for _, suffix := range sensitiveSuffixes {
		if strings.HasSuffix(n, suffix) {
			return true
		}
	}
	return false
}

// looksSensitive reports whether a value has the shape of a credential,
// regardless of what it is called.
func looksSensitive(s string) bool {
	// Cheap length gate first: this runs on every string attribute of every log
	// line, including the metrics ingest path.
	if len(s) < minSensitiveValueLen {
		return false
	}
	for _, p := range valuePrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	if looksLikeJWT(s) {
		return true
	}
	return hasEmbeddedCredential(s)
}

// looksLikeJWT matches a compact JWS. The "eyJ" prefix is base64url for '{"',
// which is also the start of any base64-encoded JSON object, so the two dots
// are what make this specific rather than a rule that blanks every encoded
// payload in the system.
func looksLikeJWT(s string) bool {
	return strings.HasPrefix(s, "eyJ") && strings.Count(s, ".") >= 2
}

// hasEmbeddedCredential spots a URL carrying userinfo with a password, such as
// postgres://user:pw@host/db. A URL without a password is left alone: hostnames
// are frequently the most useful thing in a log line.
func hasEmbeddedCredential(s string) bool {
	i := strings.Index(s, "://")
	if i < 0 {
		return false
	}
	rest := s[i+3:]
	at := strings.IndexByte(rest, '@')
	if at <= 0 {
		return false
	}
	userinfo := rest[:at]
	// A '/' before the '@' means the '@' is in the path, not the authority.
	if strings.IndexByte(userinfo, '/') >= 0 {
		return false
	}
	colon := strings.IndexByte(userinfo, ':')
	return colon > 0 && colon < len(userinfo)-1
}

// redactAttr is the slog.HandlerOptions.ReplaceAttr implementation, and handles
// leaf attributes only.
//
// slog resolves LogValuer before calling this, so a config.Secret has already
// rendered itself as the placeholder by the time it arrives here.
//
// Group *members* reach this function as individual calls, with the group path
// in the (unused) first argument, so a secret nested inside a group is caught
// normally. Group attributes themselves never reach it — slog does not offer
// them to ReplaceAttr — which is why collapsing a group logged under a
// sensitive name is handled by sanitizeGroups in the handler instead.
func redactAttr(_ []string, a slog.Attr) slog.Attr {
	if IsSensitiveKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}

	switch a.Value.Kind() {
	case slog.KindString:
		if looksSensitive(a.Value.String()) {
			return slog.String(a.Key, Redacted)
		}
	case slog.KindAny:
		// Byte slices are almost always key material, a hash, or a payload.
		// None of them belong in a log, and a hex dump of one is never what the
		// author wanted.
		if b, ok := a.Value.Any().([]byte); ok {
			if len(b) == 0 {
				return slog.String(a.Key, "")
			}
			return slog.String(a.Key, Redacted+" (bytes)")
		}
		if s, ok := a.Value.Any().(string); ok && looksSensitive(s) {
			return slog.String(a.Key, Redacted)
		}
		if err, ok := a.Value.Any().(error); ok && err != nil && looksSensitive(err.Error()) {
			return slog.String(a.Key, Redacted)
		}
	}
	return a
}

// sanitizeGroups collapses any group logged under a sensitive name.
//
// It exists because slog does NOT pass group attributes to ReplaceAttr — only
// their contents, one leaf at a time. So `slog.Group("credentials", ...)` would
// otherwise emit every member individually under names the redactor has no
// reason to suspect. This runs in the handler, where the group is still visible
// as a group.
//
// The common case allocates nothing: it reports modified=false and the caller
// keeps the original slice.
func sanitizeGroups(attrs []slog.Attr) (out []slog.Attr, modified bool) {
	for i, a := range attrs {
		var replacement slog.Attr
		switch {
		case a.Value.Kind() != slog.KindGroup:
			continue
		case IsSensitiveKey(a.Key):
			replacement = slog.String(a.Key, Redacted)
		default:
			inner, innerModified := sanitizeGroups(a.Value.Group())
			if !innerModified {
				continue
			}
			replacement = slog.Attr{Key: a.Key, Value: slog.GroupValue(inner...)}
		}

		if !modified {
			out = make([]slog.Attr, len(attrs))
			copy(out, attrs)
			modified = true
		}
		out[i] = replacement
	}
	if !modified {
		return attrs, false
	}
	return out, true
}

// anySensitiveGroup reports whether a record needs the rebuild that
// sanitizeGroups implies, without allocating.
func anySensitiveGroup(attrs []slog.Attr) bool {
	for _, a := range attrs {
		if a.Value.Kind() != slog.KindGroup {
			continue
		}
		if IsSensitiveKey(a.Key) || anySensitiveGroup(a.Value.Group()) {
			return true
		}
	}
	return false
}
