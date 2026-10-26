package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// RequestBodyLimit 使用 MaxBytesReader 限制请求体大小。
//
// 它同时把生效的上限记在 gin.Context 里（多层叠加时取最小值），供鉴权之后的在途请求体预算
// （RequestBodyBudgetMiddleware）按真实上限预占。
func RequestBodyLimit(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		applyRequestBodyLimit(c, maxBytes)
		c.Next()
	}
}

// RequestBodyLimitByRoute 和 RequestBodyLimit 一样，但按匹配到的路由模板（c.FullPath()）选上限，
// overrides 里没有的路由用 defaultMax。用在路由分组上，让组里个别路由用更小的上限，同时每个请求
// 只经过一层上限：鉴权之后的预算看到的就是最终的上限，不会先按大的预占。
func RequestBodyLimitByRoute(defaultMax int64, overrides map[string]int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		maxBytes := defaultMax
		if v, ok := overrides[c.FullPath()]; ok {
			maxBytes = v
		}
		applyRequestBodyLimit(c, maxBytes)
		c.Next()
	}
}

func applyRequestBodyLimit(c *gin.Context, maxBytes int64) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
	noteRequestBodyLimit(c, maxBytes)
}
