package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// diagnosis is diagnose_workload's result as the engine returns it: decoded JSON.
func diagnosis() any {
	var d any
	_ = json.Unmarshal([]byte(`{
		"status":{"kind":"Deployment","namespace":"team-a-api","name":"api","replicas":{"desired":3,"ready":1,"updated":3,"available":1},
			"pods":[{"name":"api-1","phase":"Running","ready":false,"restarts":7,"waiting":"CrashLoopBackOff",
				"lastTerminated":{"container":"api","reason":"OOMKilled","exitCode":137}},
				{"name":"api-2","phase":"Running","ready":true,"restarts":0}]},
		"events":[{"type":"Warning","reason":"BackOff","object":"Pod/api-1","message":"Back-off restarting failed container","count":12},
			{"type":"Normal","reason":"Pulled","object":"Pod/api-1","message":"pulled"}],
		"logs":[{"pod":"api-1","container":"api","previous":true,"lines":["2026-10-16T08:00:00Z starting","2026-10-16T08:00:01Z fatal: out of memory"]}],
		"unavailable":{"metrics":"no metrics backend"}}`), &d)
	return d
}

func note() Notification {
	return Notification{
		Alert:     Alert{Name: "KubePodCrashLooping", Severity: "critical", Summary: "api is crash looping", Fingerprint: "f1"},
		Workload:  &Workload{Environment: "staging", Namespace: "team-a-api", Kind: "Deployment", Name: "api"},
		Diagnosis: diagnosis(),
	}
}

func TestText(t *testing.T) {
	got := Text(note())
	for _, want := range []string{
		"KubePodCrashLooping", "critical", "api is crash looping", "staging", "Deployment team-a-api/api",
		"1/3 ready", "api-1: CrashLoopBackOff, 7 restarts, last OOMKilled (exit 137)",
		"BackOff (x12) Pod/api-1: Back-off restarting failed container",
		"fatal: out of memory", "metrics: no metrics backend",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("text misses %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Pulled") || strings.Contains(got, "api-2:") {
		t.Errorf("text carries what is fine:\n%s", got)
	}
	failed := Text(Notification{Alert: Alert{Name: "NodeDiskFull"}, Error: "alert not mapped to a workload: no namespace label"})
	if !strings.Contains(failed, "Not investigated: alert not mapped") {
		t.Errorf("failed text:\n%s", failed)
	}
}

func TestWebhookPostsJSONWithToken(t *testing.T) {
	var got Notification
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	err := (&Webhook{URL: srv.URL, Token: func() (string, error) { return "t0ken", nil }}).Notify(context.Background(), note())
	if err != nil || auth != "Bearer t0ken" || got.Workload.Name != "api" || got.Diagnosis == nil {
		t.Fatalf("err %v, auth %q, got %+v", err, auth, got)
	}
}

func TestSlackPostsText(t *testing.T) {
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}))
	defer srv.Close()
	err := (&Slack{URL: func() (string, error) { return srv.URL, nil }}).Notify(context.Background(), note())
	if err != nil || !strings.Contains(body["text"], "KubePodCrashLooping") {
		t.Fatalf("err %v, body %v", err, body)
	}
}

// Errors name the status, never the URL or token: a Slack URL is a credential.
func TestNotifyErrorsCarryNoCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "invalid_token")
	}))
	defer srv.Close()
	secretURL := srv.URL + "/services/T000/B000/s3cretpath"
	err := (&Slack{URL: func() (string, error) { return secretURL, nil }}).Notify(context.Background(), note())
	if err == nil || strings.Contains(err.Error(), "s3cretpath") {
		t.Fatalf("err = %v", err)
	}
}
