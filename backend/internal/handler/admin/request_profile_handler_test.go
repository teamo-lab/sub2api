package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestRequestProfileRejectsInvalidSwitchCount(t *testing.T) {
	for _, value := range []string{"3", "-1", "2,3", "0 OR 1=1", "unknown"} {
		t.Run(value, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("GET", "/profiles?"+url.Values{"switch_count": []string{value}}.Encode(), nil)
			(&OpsHandler{opsService: &service.OpsService{}}).GetRequestProfiles(c)
			if w.Code != 400 {
				t.Fatalf("status=%d", w.Code)
			}
		})
	}
}
