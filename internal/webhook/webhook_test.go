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

func TestParse(t *testing.T) {
	tests := map[string]struct {
		body        string
		headers     map[string]string
		wantEnv     string
		wantDeleted bool
		wantIgnore  bool
		wantErr     bool
	}{
		"github branch": {
			body:    `{"ref":"refs/heads/production"}`,
			headers: map[string]string{"X-GitHub-Event": "push"},
			wantEnv: "production",
		},
		"github ping": {
			body:       `{"zen":"hi"}`,
			headers:    map[string]string{"X-GitHub-Event": "ping"},
			wantIgnore: true,
		},
		"github branch delete": {
			body:        `{"ref":"refs/heads/testing","deleted":true}`,
			headers:     map[string]string{"X-GitHub-Event": "push"},
			wantEnv:     "testing",
			wantDeleted: true,
		},
		"github tag ignored": {
			body:       `{"ref":"refs/tags/v1"}`,
			headers:    map[string]string{"X-GitHub-Event": "push"},
			wantIgnore: true,
		},
		"github tag delete ignored": {
			body:       `{"ref":"refs/tags/v1","deleted":true}`,
			headers:    map[string]string{"X-GitHub-Event": "push"},
			wantIgnore: true,
		},
		"branch name sanitized": {
			body:    `{"ref":"refs/heads/feature/new-thing"}`,
			headers: map[string]string{"X-GitHub-Event": "push"},
			wantEnv: "feature_new_thing",
		},
		"deleted branch name sanitized": {
			body:        `{"ref":"refs/heads/feature/new-thing","deleted":true}`,
			headers:     map[string]string{"X-GitHub-Event": "push"},
			wantEnv:     "feature_new_thing",
			wantDeleted: true,
		},
		"gitlab push": {
			body:    `{"ref":"refs/heads/production","object_kind":"push","after":"cafe1234"}`,
			headers: map[string]string{"X-Gitlab-Event": "Push Hook"},
			wantEnv: "production",
		},
		"gitlab delete via zero after": {
			body:        `{"ref":"refs/heads/testing","object_kind":"push","after":"0000000000000000000000000000000000000000"}`,
			headers:     map[string]string{"X-Gitlab-Event": "Push Hook"},
			wantEnv:     "testing",
			wantDeleted: true,
		},
		"generic environment field": {
			body:    `{"environment":"testing"}`,
			wantEnv: "testing",
		},
		"generic deleted": {
			body:        `{"environment":"testing","deleted":true}`,
			wantEnv:     "testing",
			wantDeleted: true,
		},
		"generic ref deleted": {
			body:        `{"ref":"refs/heads/testing","deleted":true}`,
			wantEnv:     "testing",
			wantDeleted: true,
		},
		"malformed body": {
			body:    `{ not json`,
			headers: map[string]string{"X-GitHub-Event": "push"},
			wantErr: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req, body := request(t, tc.body, tc.headers)
			push, err := Parse(req, body)
			if tc.wantErr {
				if err == nil {
					t.Fatal("Parse = nil error, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse error = %v", err)
			}
			if push.Ignore != tc.wantIgnore {
				t.Errorf("Ignore = %v, want %v", push.Ignore, tc.wantIgnore)
			}
			if push.Environment != tc.wantEnv {
				t.Errorf("Environment = %q, want %q", push.Environment, tc.wantEnv)
			}
			if push.Deleted != tc.wantDeleted {
				t.Errorf("Deleted = %v, want %v", push.Deleted, tc.wantDeleted)
			}
		})
	}
}
