package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestQuotaClickHouseConfigRoundTrip(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("config-version: 8\nobservability:\n  quota:\n    clickhouse:\n      endpoint: https://unused.invalid\n      database: test\n      username: test-user\n      password: test-secret\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuotaClickHouse.Database != "test" || cfg.QuotaClickHouse.Password != "test-secret" {
		t.Fatal("config not parsed")
	}
	clone := cfg.CloneForRuntime()
	data, err := yaml.Marshal(clone)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := ParseConfigBytes(data)
	if err != nil || reloaded.QuotaClickHouse != cfg.QuotaClickHouse {
		t.Fatal("configuration did not round trip")
	}
	data, err = json.Marshal(cfg)
	if err != nil || strings.Contains(string(data), "test-secret") {
		t.Fatal("secret leaked in runtime JSON")
	}
}
