package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testSecret = "s3cr3t-webhook-key"

func request(t *testing.T, body string, headers map[string]string) (*http.Request, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhook", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, []byte(body)
}

func githubSig(body string) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestAuthenticate(t *testing.T) {
	tests := map[string]struct {
		body    string
		headers map[string]string
		wantOK  bool
	}{
		"github valid": {
			`{"ref":"refs/heads/production"}`,
			map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": githubSig(`{"ref":"refs/heads/production"}`)},
			true,
		},
		"github bad signature": {
			`{"ref":"refs/heads/production"}`,
			map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": "sha256=" + strings.Repeat("00", 32)},
			false,
		},
		"gitlab valid token": {
			`{"ref":"refs/heads/production"}`,
			map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": testSecret},
			true,
		},
		"gitlab bad token": {
			`{"ref":"refs/heads/production"}`,
			map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "wrong"},
			false,
		},
		"generic bearer": {
			`{"ref":"refs/heads/production"}`,
			map[string]string{"Authorization": "Bearer " + testSecret},
			true,
		},
		"generic missing token": {
			`{"ref":"refs/heads/production"}`,
			nil,
			false,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req, body := request(t, tc.body, tc.headers)
			err := Authenticate([]byte(testSecret), req, body)
			if tc.wantOK && err != nil {
				t.Errorf("Authenticate = %v, want nil", err)
			}
			if !tc.wantOK && err == nil {
				t.Error("Authenticate = nil, want error")
			}
		})
	}
}

func TestEnvironment(t *testing.T) {
	tests := map[string]struct {
		body       string
		headers    map[string]string
		wantEnv    string
		wantIgnore bool
		wantErr    bool
	}{
		"github branch": {
			`{"ref":"refs/heads/production"}`,
			map[string]string{"X-GitHub-Event": "push"},
			"production", false, false,
		},
		"github ping": {
			`{"zen":"hi"}`,
			map[string]string{"X-GitHub-Event": "ping"},
			"", true, false,
		},
		"github branch delete": {
			`{"ref":"refs/heads/production","deleted":true}`,
			map[string]string{"X-GitHub-Event": "push"},
			"", true, false,
		},
		"github tag ignored": {
			`{"ref":"refs/tags/v1"}`,
			map[string]string{"X-GitHub-Event": "push"},
			"", true, false,
		},
		"branch name sanitized": {
			`{"ref":"refs/heads/feature/new-thing"}`,
			map[string]string{"X-GitHub-Event": "push"},
			"feature_new_thing", false, false,
		},
		"gitlab delete via zero after": {
			`{"ref":"refs/heads/production","object_kind":"push","after":"0000000000000000000000000000000000000000"}`,
			map[string]string{"X-Gitlab-Event": "Push Hook"},
			"", true, false,
		},
		"generic environment field": {
			`{"environment":"testing"}`,
			nil,
			"testing", false, false,
		},
		"malformed body": {
			`{ not json`,
			map[string]string{"X-GitHub-Event": "push"},
			"", false, true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req, body := request(t, tc.body, tc.headers)
			env, ignore, _, err := Environment(req, body)
			if tc.wantErr {
				if err == nil {
					t.Fatal("Environment = nil error, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Environment error = %v", err)
			}
			if ignore != tc.wantIgnore {
				t.Errorf("ignore = %v, want %v", ignore, tc.wantIgnore)
			}
			if env != tc.wantEnv {
				t.Errorf("env = %q, want %q", env, tc.wantEnv)
			}
		})
	}
}
