package service

import (
	"io"
	"slices"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/relay"
	"github.com/gin-gonic/gin"
)

// This first integration deliberately shares the existing native Responses
// parser, protocol repair and error classifiers. It adds no HTTP hop or state
// store. Only the authenticated API key group and server configuration select
// exposure: an external request header cannot opt another group into the trial.
func (s *OpenAIGatewayService) teamoRelayEnabled(c *gin.Context) bool {
	return s != nil && TeamoRelayEnabledForRequest(s.cfg, c)
}

// TeamoRelayEnabledForRequest is shared with the entry observation middleware,
// which records assignment before queueing/selection can fail.
func TeamoRelayEnabledForRequest(cfg *config.Config, c *gin.Context) bool {
	if cfg == nil || !cfg.Gateway.TeamoRelayEnabled || c == nil {
		return false
	}
	value, ok := c.Get("api_key")
	if !ok {
		return false
	}
	apiKey, ok := value.(*APIKey)
	return ok && apiKey != nil && apiKey.GroupID != nil && slices.Contains(cfg.Gateway.TeamoRelayGroupIDs, *apiKey.GroupID)
}

const TeamoRelayPathsKey = "teamo_relay_paths"

func markTeamoRelayPath(c *gin.Context, path string) {
	paths, _ := c.Get(TeamoRelayPathsKey)
	list, _ := paths.([]string)
	if !slices.Contains(list, path) {
		c.Set(TeamoRelayPathsKey, append(list, path))
	}
}

type openAIStreamStage interface {
	WriteString(string) (int, error)
	Buffered() int64
	CommitTo(io.Writer) error
	Close() error
	Closed() bool
}

func (s *OpenAIGatewayService) newOpenAIStreamStage(c *gin.Context) openAIStreamStage {
	if s.teamoRelayEnabled(c) {
		markTeamoRelayPath(c, "responses")
		return relay.NewCommitGate(openAIFirstOutputStageMaxBytes)
	}
	return newDefaultOpenAIFirstOutputStage()
}

func (s *openAIFirstOutputStage) Closed() bool { return s.closed }
