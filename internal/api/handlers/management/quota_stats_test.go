package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotastats"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestQuotaStatisticsFetchAndReads(t *testing.T) {
	for _, key := range []string{"ENDPOINT", "DATABASE", "USERNAME", "PASSWORD"} {
		t.Setenv("CLICKHOUSE_QUOTA_"+key, "")
	}
	var samples []quotastats.Sample
	fail := false
	clickhouse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "private database details", 500)
			return
		}
		if strings.HasPrefix(r.URL.Query().Get("query"), "INSERT") {
			var s quotastats.Sample
			if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
				t.Error(err)
			}
			samples = append(samples, s)
		} else {
			for _, s := range samples {
				if err := json.NewEncoder(w).Encode(s); err != nil {
					t.Error(err)
				}
			}
		}
	}))
	defer clickhouse.Close()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "quota-test", Provider: "codex", FileName: "quota-test.json"}
	index := auth.EnsureIndex()
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	host := pluginhost.New()
	host.RegisterPluginForTest("quota-test", pluginapi.Plugin{Metadata: pluginapi.Metadata{Name: "quota-test"}, Capabilities: pluginapi.Capabilities{QuotaProvider: &mockQuotaProvider{identifier: "codex"}}})
	cfg := &config.Config{QuotaClickHouse: config.QuotaClickHouseConfig{Endpoint: clickhouse.URL, Database: "test"}}
	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	h.SetPluginHost(host)
	router := gin.New()
	v8 := router.Group("/v8/management")
	v8.Use(func(c *gin.Context) { c.Set(ConfigV8ContextKey, true) })
	v8.GET("/plugins/:id/quota", h.GetPluginQuota)
	v8.GET("/observability/quota/history", h.GetQuotaHistory)
	v8.GET("/observability/quota/pools", h.GetQuotaPools)
	router.GET("/v0/management/plugins/:id/quota", h.GetPluginQuota)
	request := func(path string, status int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != status {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return w
	}
	request("/v0/management/plugins/quota-test/quota?auth_index="+index, 200)
	if len(samples) != 0 {
		t.Fatal("deprecated v0 changed")
	}
	w := request("/v8/management/plugins/quota-test/quota?auth_index="+index, 200)
	if w.Header().Get("X-Quota-Statistics") != "stored" || len(samples) != 1 || samples[0].Account != index || samples[0].Remaining != 90 {
		t.Fatalf("sample not persisted: %v", samples)
	}
	request("/v8/management/plugins/missing/quota?auth_index="+index, 404)
	if len(samples) != 1 {
		t.Fatal("failed provider fetch recorded")
	}
	w = request("/v8/management/observability/quota/history", 200)
	var history struct {
		History map[string][]quotastats.Sample `json:"history"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.History["codex:"+index]) != 1 {
		t.Fatal("wrong history identity")
	}
	w = request("/v8/management/observability/quota/pools", 200)
	var pools struct {
		Pools []quotastats.Pool `json:"pools"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pools); err != nil {
		t.Fatal(err)
	}
	if len(pools.Pools) != 1 || pools.Pools[0].Remaining != 90 || pools.Pools[0].Burn != nil || pools.Pools[0].Projection.Available {
		t.Fatalf("invalid pool: %+v", pools)
	}
	for _, query := range []string{"from=bad", "to=999999999999999999", "from=1&to=999999999999", "rateHours=2", "usageMultiplier=NaN", "from=100&to=99"} {
		request("/v8/management/observability/quota/pools?"+query, 400)
	}
	fail = true
	w = request("/v8/management/plugins/quota-test/quota?auth_index="+index, 200)
	if w.Header().Get("X-Quota-Statistics") != "failed" {
		t.Fatal("ingest failure not surfaced")
	}
	w = request("/v8/management/observability/quota/history", 502)
	if strings.Contains(w.Body.String(), "private") {
		t.Fatal("database details leaked")
	}
	h.SetConfig(&config.Config{})
	request("/v8/management/observability/quota/pools", 503)
}
