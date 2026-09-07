package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Run with TEAMO_GATEWAY_MANIFEST pointing at services/teamo-gateway/Cargo.toml.
// A real HTTP client calls Gateway -> Sub2API -> scripted upstream. Account
// selection/two-attempt allowance and Router control plane are fixtures; both
// services execute their real streaming/commit logic.
func TestGatewaySub2APIConnectedHTTP(t *testing.T) {
	manifest := os.Getenv("TEAMO_GATEWAY_MANIFEST")
	if manifest == "" {
		t.Skip("set TEAMO_GATEWAY_MANIFEST to run the cross-repository HTTP chain")
	}
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		for _, mode := range []string{"success", "sub_recovery", "gateway_recovery", "exhausted", "real_output"} {
			t.Run(fmt.Sprintf("passthrough=%v/%s", passthrough, mode), func(t *testing.T) {
				prefix := "data: " + syntheticHeartbeat + "\n\n" +
					"data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n" +
					"data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"r\"}}\n\n" + emptyMessagePreamble
				failed := prefix
				if mode == "real_output" {
					failed += "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"
				}
				failed += "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"upstream processing failed\"}}}\n\n"
				good := prefix + "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\" OK\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
				var upstreamCalls, subCalls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					call := upstreamCalls.Add(1)
					body := failed
					if mode == "success" || mode == "sub_recovery" && call > 1 || mode == "gateway_recovery" && r.Header.Get("X-Test-Candidate") == "Bearer sub-premium" {
						body = good
					}
					w.Header().Set("Content-Type", "text/event-stream")
					// Split JSON/SSE across HTTP writes rather than sending a single body.
					for offset := 0; offset < len(body); offset += 7 {
						end := offset + 7
						if end > len(body) {
							end = len(body)
						}
						if _, err := io.WriteString(w, body[offset:end]); err != nil {
							return
						}
						w.(http.Flusher).Flush()
					}
				}))
				defer upstream.Close()
				svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
				engine := gin.New()
				engine.POST("/v1/responses", func(c *gin.Context) {
					subCalls.Add(1)
					for attempt := 0; attempt < 2; attempt++ {
						req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL, nil)
						if err != nil {
							c.Status(500)
							return
						}
						req.Header.Set("X-Test-Candidate", c.GetHeader("Authorization"))
						resp, err := upstream.Client().Do(req)
						if err != nil {
							c.Status(502)
							return
						}
						account := &Account{ID: int64(attempt + 1), Platform: PlatformOpenAI, Name: "mock"}
						if passthrough {
							_, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "gpt-6-astra", "gpt-6-astra")
						} else {
							_, err = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "gpt-6-astra", "gpt-6-astra")
						}
						_ = resp.Body.Close()
						if err == nil {
							return
						}
						var fe *UpstreamFailoverError
						if !errors.As(err, &fe) {
							return
						}
						if attempt == 1 {
							// Fixed exhausted-account response; stream decisions remain real.
							c.JSON(503, gin.H{"error": gin.H{"code": "server_error", "message": "upstream processing failed"}})
							return
						}
					}
				})
				sub := httptest.NewServer(engine)
				defer sub.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				cmd := exec.CommandContext(ctx, "cargo", "test", "--offline", "--manifest-path", manifest, "--bin", "teamo-gateway", "direct_relay::tests::connected_sub2api_http_chain", "--", "--ignored", "--exact", "--nocapture")
				cmd.Env = append(os.Environ(), "TEAMO_CHAIN_SUB_URL="+sub.URL, "TEAMO_CHAIN_MODE="+mode, "TEAMO_CHAIN_GOOD="+good)
				output, err := cmd.CombinedOutput()
				require.NoError(t, err, "%s", output)
				require.Contains(t, string(output), "CONNECTED_CHAIN mode="+mode)
				expectedSub, expectedUp := int32(1), int32(1)
				switch mode {
				case "sub_recovery":
					expectedUp = 2
				case "gateway_recovery":
					expectedSub = 2
					expectedUp = 3
				case "exhausted":
					expectedSub = 2
					expectedUp = 4
				}
				require.Equal(t, expectedSub, subCalls.Load())
				require.Equal(t, expectedUp, upstreamCalls.Load())
				for _, line := range strings.Split(string(output), "\n") {
					if strings.Contains(line, "CONNECTED_CHAIN") {
						t.Log(line)
					}
				}
				t.Logf("sub_requests=%d upstream_requests=%d", subCalls.Load(), upstreamCalls.Load())
			})
		}
	}
}
