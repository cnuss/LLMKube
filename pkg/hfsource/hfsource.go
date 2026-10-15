/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package hfsource holds the Hugging Face source resolution shared by the
// Model controller and the Metal agent. Before #1759 the resolver lived only
// in internal/controller, so the agent handed Model.Spec.Source verbatim to
// net/http and every hf:// Model failed there with `unsupported protocol
// scheme "hf"`.
package hfsource

import (
	"fmt"
	"net/url"
	"strings"
)

// HasSchemeFold reports whether source starts with the given scheme prefix
// (e.g. "http://"), matching case-insensitively. URL schemes are
// case-insensitive per RFC 3986 §3.1 and url.Parse lowercases them, so the
// source classifiers must agree with the URL parser: a case-sensitive match
// would let a case-variant scheme ("HTTP://...") dodge its classifier and
// fall through to a differently-guarded code path (GHSA-jw3m-8q7m-f35r).
func HasSchemeFold(source, prefix string) bool {
	return len(source) >= len(prefix) && strings.EqualFold(source[:len(prefix)], prefix)
}

// IsHuggingFaceURL reports whether source is a huggingface.co URL
// (https://huggingface.co/... or http://huggingface.co/...).
func IsHuggingFaceURL(source string) bool {
	if !HasSchemeFold(source, "https://") && !HasSchemeFold(source, "http://") {
		return false
	}
	rest := source
	if HasSchemeFold(rest, "https://") {
		rest = rest[len("https://"):]
	} else {
		rest = rest[len("http://"):]
	}
	// Host is case-insensitive per RFC 3986; the repo path is not (Qwen/... must
	// keep its case), so only lower-case for the host comparison. Fold BEFORE
	// trimming the www. label: trimming first leaves "WWW.huggingface.co"
	// untouched and the match fails, which for the auth gate means downloading
	// unauthenticated rather than sending the token. It fails safe, but the code
	// disagreed with this comment.
	rest = strings.ToLower(rest)
	rest = strings.TrimPrefix(rest, "www.")
	return strings.HasPrefix(rest, "huggingface.co/")
}

// IsHFAuthSource reports whether the operator's own downloads for this source
// should carry a Hugging Face bearer token. True for both spellings the
// downloader accepts: an hf:// source, which the Metal agent resolves to
// huggingface.co with NormalizeHFSource and the init container rewrites with
// normalize_hf_source, and a literal huggingface.co URL.
//
// This is the gate that keeps the token off every other host (#1750). The
// header is emitted only when this returns true, so a Model pointing at a
// mirror, a private registry, or an arbitrary https:// URL never sees it, even
// if the referenced Secret happens to carry an HF_TOKEN key.
func IsHFAuthSource(source string) bool {
	return HasSchemeFold(source, "hf://") || IsHuggingFaceURL(source)
}

// IsHFAuthSourceForEndpoint reports whether the operator's own downloads for
// source should carry a Hugging Face bearer token, given the HF_ENDPOINT URL
// named by the same spec.sourceSecretRef that holds the token. It is
// IsHFAuthSource plus the mirror case: a source on the origin the user named.
//
// What the Secret authorizes is an ORIGIN: scheme, host and effective port.
// A Model author who controls only spec.source can therefore not redirect the
// token to a different service on that host, nor downgrade it to cleartext: an
// http:// source does not match an https:// endpoint, and :8080 does not match
// the scheme default. The path is not compared, so an Artifactory endpoint with
// a long /artifactory/api/... path still matches its own resolve URLs, and the
// match is exact, so mirror.corp.example.evil.com never sees the token.
//
// An empty HF_ENDPOINT names no mirror, so only IsHFAuthSource's hosts qualify.
func IsHFAuthSourceForEndpoint(source, hfEndpoint string) bool {
	if IsHFAuthSource(source) {
		return true
	}
	return IsOnEndpoint(source, hfEndpoint)
}

// IsOnEndpoint reports whether source is on the origin the HF_ENDPOINT URL
// names, by the same exact origin rule IsHFAuthSourceForEndpoint applies to the
// token. An empty or scheme-less HF_ENDPOINT names no origin and matches nothing.
func IsOnEndpoint(source, hfEndpoint string) bool {
	return sameOrigin(source, hfEndpoint)
}

// sameOrigin reports whether two absolute URLs name the same origin: equal
// scheme (case-insensitive), equal host (case-insensitive), and equal effective
// port. The port is the URL's own, or the scheme default when it is omitted, so
// https://h and https://h:443 are the same origin while https://h:8080 is not.
//
// A value without a scheme ("mirror.corp.example") parses as a path, so it
// names no origin and matches nothing. That fails safe: the download goes out
// unauthenticated rather than the token reaching an origin the user did not
// spell out in full, matching how huggingface_hub writes HF_ENDPOINT.
func sameOrigin(a, b string) bool {
	aScheme, aHost, aPort, aOK := sourceOrigin(a)
	bScheme, bHost, bPort, bOK := sourceOrigin(b)
	if !aOK || !bOK {
		return false
	}
	return aScheme == bScheme && aHost == bHost && aPort == bPort
}

// sourceOrigin splits an absolute URL into the origin triple the mirror gate
// compares. ok is false when raw is empty or does not parse as a URL with a
// host. The path, query, fragment and userinfo are deliberately ignored: a
// Hugging Face remote in Artifactory names its endpoint with a long
// /artifactory/api/... path, and its resolve URLs share the origin with it.
func sourceOrigin(raw string) (scheme, host, port string, ok bool) {
	if raw == "" {
		return "", "", "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", "", false
	}
	h := strings.ToLower(u.Hostname())
	if h == "" {
		return "", "", "", false
	}
	return strings.ToLower(u.Scheme), h, effectivePort(u), true
}

// effectivePort returns the URL's port, or the scheme's default when the URL
// omits it, so an explicit :443 and a bare https:// compare equal.
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// HFURLPathSegments returns the non-empty path segments after the
// "huggingface.co/" host for a huggingface.co URL, with any query string or
// fragment stripped. ok is false when source is not a huggingface.co URL.
// The host comparison is case-insensitive (RFC 3986) but the returned segments
// preserve their original case, since HF repo names are case-sensitive.
func HFURLPathSegments(source string) (segments []string, ok bool) {
	rest := source
	if HasSchemeFold(rest, "https://") {
		rest = rest[len("https://"):]
	} else if HasSchemeFold(rest, "http://") {
		rest = rest[len("http://"):]
	} else {
		return nil, false
	}
	// Fold ONLY the host, up to the first slash: the host is case-insensitive
	// per RFC 3986 but the repo path is not, so Qwen/Qwen3-8B must keep its
	// capital Q. Folding has to happen before the www. trim, or an uppercase
	// label survives and the match below fails.
	//
	// This has to agree with IsHuggingFaceURL. When it did not, an uppercase
	// host classified as a Hugging Face URL there and as nothing here, leaving
	// a source that was neither an HF repo nor a plain remote HTTP file, which
	// no branch in the Model controller's classifier handles.
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = strings.ToLower(rest[:i]) + rest[i:]
	}
	rest = strings.TrimPrefix(rest, "www.")
	// "huggingface.co/" is 15 bytes in any case, and the host is folded above,
	// so slicing by the literal length is safe.
	if !strings.HasPrefix(rest, "huggingface.co/") {
		return nil, false
	}
	rest = rest[len("huggingface.co/"):]
	// Browser pastes routinely carry "?library=vllm" or "#..." which would
	// otherwise be glued onto the repo name.
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	for _, s := range strings.Split(rest, "/") {
		if s != "" {
			segments = append(segments, s)
		}
	}
	return segments, true
}

// IsHuggingFaceFileURL reports whether source is a huggingface.co URL that names
// a specific file, e.g. .../resolve/<rev>/<file> or .../blob/<rev>/<file>. Such
// URLs are single-file downloads, not repo references.
func IsHuggingFaceFileURL(source string) bool {
	clean, ok := HFURLPathSegments(source)
	if !ok || len(clean) < 5 {
		return false
	}
	return clean[2] == "resolve" || clean[2] == "blob"
}

// ExtractHFRepoFromURL extracts the repo ID and optional revision from a
// huggingface.co REPO URL (landing page, /tree/<rev>, or a revision-pinned
// resolve/blob root). It returns ok=false for datasets/spaces pages and for
// URLs that name a specific file (/resolve|blob/<rev>/<file>), which are
// single-file downloads rather than repo references.
func ExtractHFRepoFromURL(source string) (repoID, revision string, ok bool) {
	clean, isURL := HFURLPathSegments(source)
	if !isURL || len(clean) < 2 {
		return "", "", false
	}
	if clean[0] == "datasets" || clean[0] == "spaces" {
		return "", "", false
	}
	repoID = clean[0] + "/" + clean[1]
	if len(clean) >= 3 {
		switch clean[2] {
		case "tree":
			if len(clean) >= 4 {
				revision = clean[3]
			}
		case "resolve", "blob":
			// A resolve/blob URL naming a specific file is a single-file
			// download, not a repo; leave it to the caller's remote-HTTP
			// classifier. A resolve/blob root with only a revision is a
			// revision-pinned repo.
			if len(clean) >= 5 {
				return "", "", false
			}
			if len(clean) >= 4 {
				revision = clean[3]
			}
		}
	}
	return repoID, revision, true
}

// ParseHFSource splits an HF source into repo ID and optional revision.
// Accepts "hf://org/repo@rev", "org/repo@rev", and
// "https://huggingface.co/org/repo[/tree/rev|...]" forms.
// Returns (repoID, revision, error) where revision is "" if not specified.
func ParseHFSource(source string) (repoID, revision string, err error) {
	if IsHuggingFaceURL(source) {
		repoID, revision, ok := ExtractHFRepoFromURL(source)
		if !ok {
			return "", "", fmt.Errorf("invalid huggingface.co URL: %s", source)
		}
		if repoID == "" {
			return "", "", fmt.Errorf("empty repo ID in hf source: %s", source)
		}
		if revision != "" && strings.ContainsAny(revision, " \t\n\r") {
			return "", "", fmt.Errorf("hf revision must not contain whitespace: %s", source)
		}
		return repoID, revision, nil
	}
	normalized := strings.TrimPrefix(source, "hf://")
	if normalized == "" {
		return "", "", fmt.Errorf("empty hf repo source: %s", source)
	}

	// Split on @ to extract revision
	atIdx := strings.Index(normalized, "@")
	if atIdx >= 0 {
		repoID = normalized[:atIdx]
		revision = normalized[atIdx+1:]
		if repoID == "" {
			return "", "", fmt.Errorf("empty repo ID in hf source: %s", source)
		}
		if revision == "" {
			return "", "", fmt.Errorf("empty revision in hf source: %s", source)
		}
		// Reject whitespace in revision (common user error)
		if strings.ContainsAny(revision, " \t\n\r") {
			return "", "", fmt.Errorf("hf revision must not contain whitespace: %s", source)
		}
		return repoID, revision, nil
	}

	// No @rev specified
	repoID = normalized
	if repoID == "" {
		return "", "", fmt.Errorf("empty repo ID in hf source: %s", source)
	}
	return repoID, "", nil
}

// NormalizeHFSource converts an HF source to its full HTTPS resolve URL.
// For hf://org/repo@rev, returns "https://huggingface.co/org/repo/resolve/rev/".
// For hf://org/repo (no rev), returns "https://huggingface.co/org/repo/resolve/main/".
// For https://huggingface.co/org/repo[/tree/rev|...], returns the equivalent
// resolve URL. Non-hf sources pass through unchanged.
func NormalizeHFSource(source string) string {
	if IsHuggingFaceURL(source) {
		repoID, revision, err := ParseHFSource(source)
		if err != nil {
			return source
		}
		if revision == "" {
			revision = "main"
		}
		return fmt.Sprintf("https://huggingface.co/%s/resolve/%s/", repoID, revision)
	}
	if !strings.HasPrefix(strings.ToLower(source), "hf://") {
		return source
	}
	repoID, revision, err := ParseHFSource(source)
	if err != nil {
		// On parse error, return the original source; validation will catch it.
		return source
	}
	if revision == "" {
		revision = "main"
	}
	return fmt.Sprintf("https://huggingface.co/%s/resolve/%s/", repoID, revision)
}
