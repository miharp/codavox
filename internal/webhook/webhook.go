// Package webhook parses and authenticates control-repo push notifications.
//
// It is a pure request mapper: given a push from GitHub, GitLab, or a generic
// caller, it verifies the shared secret and reports which environment to
// deploy. It does not deploy or hold state — the deploy server owns the queue,
// the worker, and the history — so the whole parse-and-authenticate path is
// testable on its own.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/miharp/codavox/internal/layout"
)

// MaxBodyBytes caps a payload the caller should read. GitHub documents a 25 MiB
// maximum, and the whole body must be read to verify the HMAC, so this is that
// ceiling.
const MaxBodyBytes = 25 << 20

// Authenticate verifies the request against the shared secret, choosing the
// scheme by provider: GitHub's HMAC over the body, or GitLab's and the generic
// caller's token.
func Authenticate(secret []byte, r *http.Request, body []byte) error {
	switch detectProvider(r) {
	case "github":
		return verifyGitHubSignature(secret, body, r.Header.Get("X-Hub-Signature-256"))
	case "gitlab":
		if !secretEqual(secret, []byte(r.Header.Get("X-Gitlab-Token"))) {
			return errors.New("gitlab token mismatch")
		}
		return nil
	default:
		if !secretEqual(secret, []byte(genericToken(r))) {
			return errors.New("token mismatch")
		}
		return nil
	}
}

// Environment maps a push to the environment to deploy. ignore is true for
// events that are valid but deploy nothing — pings, non-push events, branch
// deletions, and non-branch refs — with reason describing why.
func Environment(r *http.Request, body []byte) (env string, ignore bool, reason string, err error) {
	ev, err := parseEvent(detectProvider(r), r, body)
	if err != nil {
		return "", false, "", err
	}
	if ev.ignore {
		return "", true, ev.reason, nil
	}
	env, err = environmentFromRef(ev.ref)
	if err != nil {
		return "", false, "", err
	}
	return env, false, "", nil
}

func detectProvider(r *http.Request) string {
	switch {
	case r.Header.Get("X-GitHub-Event") != "":
		return "github"
	case r.Header.Get("X-Gitlab-Event") != "":
		return "gitlab"
	default:
		return "generic"
	}
}

// verifyGitHubSignature checks GitHub's HMAC-SHA256 of the body. The secret is
// never on the wire, so this holds even without TLS.
func verifyGitHubSignature(secret, body []byte, header string) error {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return errors.New("missing sha256 signature")
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return errors.New("malformed signature")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if !hmac.Equal(want, mac.Sum(nil)) {
		return errors.New("signature mismatch")
	}
	return nil
}

func genericToken(r *http.Request) string {
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func secretEqual(a, b []byte) bool {
	// ConstantTimeCompare returns 0 on a length mismatch, so an empty presented
	// token never matches a configured secret.
	return subtle.ConstantTimeCompare(a, b) == 1
}

type event struct {
	ref    string
	ignore bool
	reason string
}

func parseEvent(provider string, r *http.Request, body []byte) (event, error) {
	switch provider {
	case "github":
		switch e := r.Header.Get("X-GitHub-Event"); e {
		case "push":
		case "ping":
			return event{ignore: true, reason: "ping"}, nil
		default:
			return event{ignore: true, reason: "non-push event " + e}, nil
		}
		var p struct {
			Ref     string `json:"ref"`
			Deleted bool   `json:"deleted"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return event{}, fmt.Errorf("parsing github payload: %w", err)
		}
		if p.Deleted {
			return event{ignore: true, reason: "branch deleted"}, nil
		}
		return branchEvent(p.Ref), nil

	case "gitlab":
		var p struct {
			Ref        string `json:"ref"`
			ObjectKind string `json:"object_kind"`
			After      string `json:"after"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return event{}, fmt.Errorf("parsing gitlab payload: %w", err)
		}
		if p.ObjectKind != "" && p.ObjectKind != "push" {
			return event{ignore: true, reason: "non-push event " + p.ObjectKind}, nil
		}
		// A branch deletion sends an all-zero "after" sha.
		if p.After != "" && strings.Trim(p.After, "0") == "" {
			return event{ignore: true, reason: "branch deleted"}, nil
		}
		return branchEvent(p.Ref), nil

	default:
		var p struct {
			Ref         string `json:"ref"`
			Environment string `json:"environment"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return event{}, fmt.Errorf("parsing payload: %w", err)
		}
		if p.Environment != "" {
			return event{ref: "refs/heads/" + p.Environment}, nil
		}
		if p.Ref == "" {
			return event{}, errors.New("payload has neither ref nor environment")
		}
		return branchEvent(p.Ref), nil
	}
}

// branchEvent ignores refs that are not branches: tags and other refs do not
// name environments.
func branchEvent(ref string) event {
	if !strings.HasPrefix(ref, "refs/heads/") {
		return event{ignore: true, reason: "non-branch ref " + ref}
	}
	return event{ref: ref}
}

var nonWord = regexp.MustCompile(`\W`)

// environmentFromRef maps a branch ref to an environment name, sanitizing \W to
// _ the way r10k does when it names environments, so the result matches the
// directory r10k deploys.
func environmentFromRef(ref string) (string, error) {
	branch := strings.TrimPrefix(ref, "refs/heads/")
	if branch == "" {
		return "", errors.New("empty branch ref")
	}
	env := nonWord.ReplaceAllString(branch, "_")
	if err := layout.ValidateEnvironment(env); err != nil {
		return "", err
	}
	return env, nil
}
