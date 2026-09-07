//go:build unit

package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
)

func TestOpenAIContentAuditTerminalPassthrough(t *testing.T) {
	body := []byte(`{"error":{"code":"content_policy_violation","message":"内容审计命中风险规则，请调整输入后重试"}}`)
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "started_sse"}[stream], func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			if stream {
				c.Writer.Header().Set("Content-Type", "text/event-stream")
				c.Writer.WriteHeaderNow()
			}
			e := &service.UpstreamFailoverError{StatusCode: 403, ResponseBody: body, Reason: service.OpenAIContentAuditRejectedReason, ClientMessage: "内容审计命中风险规则，请调整输入后重试", NextAccountAction: service.NextAccountStop}
			(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, e, stream)
			if stream {
				require.Contains(t, rec.Body.String(), "内容审计命中风险规则")
				require.NotContains(t, rec.Body.String(), "authentication")
			} else {
				require.Equal(t, 403, rec.Code)
				require.JSONEq(t, string(body), rec.Body.String())
			}
		})
	}
}
