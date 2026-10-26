// Package quotastats stores normalized quota observations, not request token usage.
package quotastats

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

const MaxSamples = 100000

var ErrTooManySamples = errors.New("quota history exceeds 100000 samples; narrow the range or filter by provider/account")

type Sample struct {
	Provider    string   `json:"provider"`
	Account     string   `json:"account"`
	At          int64    `json:"at"`
	Remaining   float64  `json:"remaining"`
	ResetAt     *int64   `json:"resetAt"`
	Window      string   `json:"window"`
	PeriodHours *float64 `json:"periodHours"`
}

func (s Sample) Name() string { return s.Provider + ":" + s.Account }

type Query struct {
	From     int64
	To       int64
	Provider string
	Account  string
}

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type ClickHouse struct {
	cfg    config.QuotaClickHouseConfig
	client HTTPClient
}

// New returns nil when disabled. It does not connect or create schema.
func New(cfg config.QuotaClickHouseConfig, client HTTPClient) (*ClickHouse, error) {
	for name, field := range map[string]*string{
		"ENDPOINT": &cfg.Endpoint, "DATABASE": &cfg.Database,
		"USERNAME": &cfg.Username, "PASSWORD": &cfg.Password,
	} {
		if value := os.Getenv("CLICKHOUSE_QUOTA_" + name); value != "" {
			*field = value
		}
	}
	if cfg.Endpoint == "" {
		return nil, nil
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid quota ClickHouse HTTP endpoint (credentials and query parameters must be configured separately)")
	}
	if cfg.Database == "" {
		return nil, errors.New("quota ClickHouse database is required")
	}
	if client == nil {
		// No network timeouts after connection establishment. Cancellation follows the caller.
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &ClickHouse{cfg: cfg, client: client}, nil
}

func (s *ClickHouse) request(ctx context.Context, query string, params url.Values, data []byte) ([]byte, error) {
	u, _ := url.Parse(s.cfg.Endpoint)
	if params == nil {
		params = url.Values{}
	}
	params.Set("database", s.cfg.Database)
	params.Set("query", query)
	params.Set("wait_end_of_query", "1")
	u.RawQuery = params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("cannot build quota ClickHouse request")
	}
	req.SetBasicAuth(s.cfg.Username, s.cfg.Password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, errors.New("quota ClickHouse request failed")
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Warn("quota ClickHouse response close failed")
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("quota ClickHouse returned HTTP %d", resp.StatusCode)
	}
	const maxBytes = 64 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, errors.New("cannot read quota ClickHouse response")
	}
	if len(body) > maxBytes {
		return nil, ErrTooManySamples
	}
	return body, nil
}

func (s *ClickHouse) Insert(ctx context.Context, sample Sample) error {
	if !validSample(sample) {
		return errors.New("invalid quota sample")
	}
	data, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	body, err := s.request(ctx, "INSERT INTO quota_samples (provider, account, at, remaining, resetAt, window, periodHours) FORMAT JSONEachRow", nil, data)
	// ClickHouse can report an exception in the body after sending HTTP 200.
	if err == nil && len(bytes.TrimSpace(body)) != 0 {
		return errors.New("quota ClickHouse insert failed")
	}
	return err
}

func (s *ClickHouse) Read(ctx context.Context, q Query) ([]Sample, error) {
	params := url.Values{"param_from": {strconv.FormatInt(q.From, 10)}, "param_to": {strconv.FormatInt(q.To, 10)}}
	query := "SELECT provider, account, at, remaining, resetAt, window, periodHours FROM quota_samples FINAL WHERE at >= {from:Int64} AND at <= {to:Int64}"
	if q.Provider != "" {
		query += " AND provider = {provider:String}"
		params.Set("param_provider", q.Provider)
	}
	if q.Account != "" {
		query += " AND account = {account:String}"
		params.Set("param_account", q.Account)
	}
	query += " ORDER BY provider, account, at LIMIT 100001 FORMAT JSONEachRow"
	params.Set("output_format_json_quote_64bit_integers", "0")
	body, err := s.request(ctx, query, params, nil)
	if err != nil {
		return nil, err
	}
	rows := make([]Sample, 0)
	decoder := json.NewDecoder(bytes.NewReader(body))
	for {
		var sample Sample
		if errDecode := decoder.Decode(&sample); errDecode == io.EOF {
			break
		} else if errDecode != nil {
			return nil, errors.New("invalid quota ClickHouse response")
		}
		if !validSample(sample) {
			return nil, errors.New("invalid stored quota sample")
		}
		rows = append(rows, sample)
		if len(rows) > MaxSamples {
			return nil, ErrTooManySamples
		}
	}
	return rows, nil
}
