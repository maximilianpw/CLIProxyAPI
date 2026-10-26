package config

// QuotaClickHouseConfig uses ClickHouse's HTTP interface. Empty endpoint disables it.
// Non-empty CLICKHOUSE_QUOTA_* environment variables override the corresponding fields.
type QuotaClickHouseConfig struct {
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	Database string `yaml:"database" json:"database"`
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"-"`
}
