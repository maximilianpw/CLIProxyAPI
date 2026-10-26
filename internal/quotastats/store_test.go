package quotastats

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"ENDPOINT", "DATABASE", "USERNAME", "PASSWORD"} {
		t.Setenv("CLICKHOUSE_QUOTA_"+key, "")
	}
}

func TestClickHouseRoundTrip(t *testing.T) {
	clearEnv(t)
	want := Sample{Provider: "codex", Account: "a' OR 1=1 --", At: 1793016000000, Remaining: 23.5, ResetAt: ptr(int64(1793102400000)), Window: "weekly", PeriodHours: ptr(168.0)}
	var stored Sample
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "test-user" || password != "test-password" || r.URL.Query().Get("database") != "test" {
			t.Error("missing connection settings")
		}
		q := r.URL.Query().Get("query")
		if strings.HasPrefix(q, "INSERT") {
			if err := json.NewDecoder(r.Body).Decode(&stored); err != nil {
				t.Error(err)
			}
		} else {
			if strings.Contains(q, want.Account) || !strings.Contains(q, "account = {account:String}") || r.URL.Query().Get("param_account") != want.Account {
				t.Error("account filter is not parameterized")
			}
			if !strings.Contains(q, " FINAL ") || r.URL.Query().Get("output_format_json_quote_64bit_integers") != "0" {
				t.Error("missing deduplication/number format")
			}
			if err := json.NewEncoder(w).Encode(stored); err != nil {
				t.Error(err)
			}
		}
	}))
	defer server.Close()
	store, err := New(config.QuotaClickHouseConfig{Endpoint: server.URL, Database: "test", Username: "test-user", Password: "test-password"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Insert(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Read(context.Background(), Query{From: want.At - 1, To: want.At + 1, Provider: want.Provider, Account: want.Account})
	if err != nil || !reflect.DeepEqual(got, []Sample{want}) {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

type doFunc func(*http.Request) (*http.Response, error)

func (f doFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestClickHouseFailuresAndConfiguration(t *testing.T) {
	clearEnv(t)
	if store, err := New(config.QuotaClickHouseConfig{}, nil); store != nil || err != nil {
		t.Fatal("not disabled")
	}
	for _, endpoint := range []string{"not-a-url", "ftp://invalid", "http://user:password@invalid", "http://invalid?password=secret"} {
		if _, err := New(config.QuotaClickHouseConfig{Endpoint: endpoint, Database: "test"}, nil); err == nil {
			t.Fatalf("accepted %s", endpoint)
		}
	}
	t.Setenv("CLICKHOUSE_QUOTA_DATABASE", "test")
	t.Setenv("CLICKHOUSE_QUOTA_ENDPOINT", "http://unused.invalid")
	for _, tc := range []struct {
		name, body string
		status     int
		transport  bool
		tooMany    bool
	}{
		{"HTTP error", "sensitive upstream message", 500, false, false},
		{"decode error", "not JSON", 200, false, false},
		{"invalid sample", `{"provider":"codex","account":"a","at":1,"remaining":101}`, 200, false, false},
		{"transport", "", 0, true, false},
		{"too many", strings.Repeat("{\"provider\":\"codex\",\"account\":\"a\",\"at\":1,\"remaining\":1}\n", MaxSamples+1), 200, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := New(config.QuotaClickHouseConfig{}, doFunc(func(r *http.Request) (*http.Response, error) {
				if tc.transport {
					return nil, errors.New("sensitive URL")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.Read(context.Background(), Query{From: 1, To: 2})
			if err == nil || strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("unsafe or missing error: %v", err)
			}
			if tc.tooMany && !errors.Is(err, ErrTooManySamples) {
				t.Fatal(err)
			}
			if tc.name == "decode error" {
				if err := store.Insert(context.Background(), Sample{Provider: "codex", Account: "a", At: 1, Remaining: 50}); err == nil {
					t.Fatal("HTTP 200 with an exception body reported as a successful insert")
				}
			}
		})
	}
}
