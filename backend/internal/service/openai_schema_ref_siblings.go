package service

import (
	"slices"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
)

// stripOpenAISchemaRefSiblingsForGroup applies the $ref annotation-sibling
// strip only to groups on the server-side allowlist. It is a compatibility
// pass: on a parse failure the original body is forwarded unchanged so the
// upstream reports the real error instead of the gateway inventing one.
func (s *OpenAIGatewayService) stripOpenAISchemaRefSiblingsForGroup(c *gin.Context, body []byte) []byte {
	if s == nil || s.cfg == nil || len(s.cfg.Gateway.OpenAIStripSchemaRefSiblingsGroupIDs) == 0 {
		return body
	}
	groupID := getOpenAIGroupIDFromContext(c)
	if groupID <= 0 || !slices.Contains(s.cfg.Gateway.OpenAIStripSchemaRefSiblingsGroupIDs, groupID) {
		return body
	}
	stripped, changed, err := stripOpenAISchemaRefAnnotationSiblings(body)
	if err != nil {
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Skip $ref sibling strip for group %d: %v", groupID, err)
		return body
	}
	if !changed {
		return body
	}
	logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Stripped $ref annotation siblings from request schema (group: %d)", groupID)
	return stripped
}
