package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// RequestBodyLimit 使用 MaxBytesReader 限制请求体大小。
//
// 它同时把生效的上限记在 gin.Context 里（多层叠加时取最小值），供鉴权之后的在途请求体预算
// （RequestBodyBudgetMiddleware）按真实上限预占；已经按更大上限预占过的，这里会缩到新的上限。
func RequestBodyLimit(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		noteRequestBodyLimit(c, maxBytes)
		c.Next()
	}
}
