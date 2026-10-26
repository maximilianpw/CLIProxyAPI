package management

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotastats"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

func (h *Handler) quotaStatsStore() (*quotastats.ClickHouse, error) {
	var cfg config.QuotaClickHouseConfig
	if snapshot := h.configSnapshot(); snapshot != nil {
		cfg = snapshot.QuotaClickHouse
	}
	return quotastats.New(cfg, nil)
}

// recordQuotaSample is called only by v8 quota fetches. Persistence failure must
// not turn a successful provider response into a failed quota refresh.
func (h *Handler) recordQuotaSample(c *gin.Context, provider, account string, response pluginapi.QuotaFetchResponse) {
	store, err := h.quotaStatsStore()
	status := "disabled"
	if err != nil {
		status = "failed"
	} else if store != nil {
		sample, ok := quotastats.Normalize(provider, account, time.Now(), response)
		status = "skipped"
		if ok {
			err = store.Insert(c.Request.Context(), sample)
			status = "stored"
			if err != nil {
				status = "failed"
			}
		}
	}
	if err != nil {
		log.Warn("quota statistics sample was not persisted")
	}
	c.Header("X-Quota-Statistics", status)
}

// GetQuotaHistory returns observations with the fleet PoolHistory key convention.
func (h *Handler) GetQuotaHistory(c *gin.Context) { h.getQuotaStats(c, false) }

// GetQuotaPools returns provider pools and explicitly qualified projections.
func (h *Handler) GetQuotaPools(c *gin.Context) { h.getQuotaStats(c, true) }

func (h *Handler) getQuotaStats(c *gin.Context, pools bool) {
	now := time.Now().UnixMilli()
	q := quotastats.Query{To: now, Provider: c.Query("provider"), Account: c.Query("account")}
	parseTime := func(key string, fallback int64) (int64, error) {
		value, exists := c.GetQuery(key)
		if !exists {
			return fallback, nil
		}
		return strconv.ParseInt(value, 10, 64)
	}
	var err error
	q.To, err = parseTime("to", now)
	if err != nil || q.To <= 0 || q.To > now {
		c.JSON(http.StatusBadRequest, gin.H{"error": "to must be a positive epoch millisecond timestamp not in the future"})
		return
	}
	q.From, err = parseTime("from", q.To-int64(24*time.Hour/time.Millisecond))
	if err != nil || q.From <= 0 || q.From >= q.To || q.To-q.From > int64(31*24*time.Hour/time.Millisecond) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "from/to must delimit a positive interval of at most 31 days"})
		return
	}
	hours, err := strconv.Atoi(c.DefaultQuery("rateHours", "3"))
	if err != nil || (hours != 1 && hours != 3 && hours != 6) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "rateHours must be 1, 3 or 6"})
		return
	}
	multiplier, err := strconv.ParseFloat(c.DefaultQuery("usageMultiplier", "1"), 64)
	if err != nil || (multiplier != 0 && multiplier != 0.5 && multiplier != 1 && multiplier != 2) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usageMultiplier must be 0, 0.5, 1 or 2"})
		return
	}
	store, err := h.quotaStatsStore()
	if err != nil || store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "quota statistics ClickHouse is disabled or misconfigured"})
		return
	}
	samples, err := store.Read(c.Request.Context(), q)
	if err != nil {
		if errors.Is(err, quotastats.ErrTooManySamples) {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		} else {
			c.JSON(http.StatusBadGateway, gin.H{"error": "quota statistics storage unavailable"})
		}
		return
	}
	c.Header("Cache-Control", "no-store")
	if pools {
		c.JSON(http.StatusOK, gin.H{"from": q.From, "to": q.To, "rateHours": hours, "usageMultiplier": multiplier, "pools": quotastats.Pools(samples, q.To, hours, multiplier)})
	} else {
		c.JSON(http.StatusOK, gin.H{"from": q.From, "to": q.To, "history": quotastats.History(samples)})
	}
}
