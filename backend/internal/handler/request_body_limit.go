package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/config"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/gin-gonic/gin"
)

func extractMaxBytesError(err error) (*http.MaxBytesError, bool) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return maxErr, true
	}
	return nil, false
}

// requestBodyBudgetExceeded 判断读请求体失败是不是因为全局在途请求体预算不够（见
// middleware.RequestBodyLimitWithBudget）。是的话先写好 Retry-After 和 Connection: close，
// 调用方再按自己的协议格式回 503。
func requestBodyBudgetExceeded(c *gin.Context, err error) bool {
	if !errors.Is(err, pkghttputil.ErrRequestBodyBudgetExceeded) {
		return false
	}
	c.Header("Retry-After", strconv.Itoa(pkghttputil.RequestBodyBudgetRetryAfterSeconds))
	c.Header("Connection", "close")
	return true
}

func formatBodyLimit(limit int64) string {
	const mb = 1024 * 1024
	if limit >= mb {
		return fmt.Sprintf("%dMB", limit/mb)
	}
	return fmt.Sprintf("%dB", limit)
}

func buildBodyTooLargeMessage(limit int64) string {
	return fmt.Sprintf("Request body too large, limit is %s", formatBodyLimit(limit))
}

func readLenientJSONRequestBodyWithPrealloc(req *http.Request, cfg *config.Config) ([]byte, error) {
	return pkghttputil.ReadLenientJSONRequestBodyWithPrealloc(req, gatewayMaxBodySize(cfg))
}

func gatewayMaxBodySize(cfg *config.Config) int64 {
	if cfg == nil {
		return 0
	}
	return cfg.Gateway.MaxBodySize
}
