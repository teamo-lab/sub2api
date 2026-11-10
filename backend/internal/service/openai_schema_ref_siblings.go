package service

import (
	"bytes"
	"slices"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
)

// applyOpenAISchemaRefCompatForGroup applies the $ref compatibility passes
// enabled for the request's group (each has its own server-side allowlist):
//   - strip annotation siblings next to $ref ("$ref cannot have keywords");
//   - repair $ref values that are not URI references (`"..." is not a
//     "uri-reference"`, e.g. Springfox "#/components/schemas/Page«User»").
//
// It is a compatibility pass: on a parse failure the original body is
// forwarded unchanged so the upstream reports the real error instead of the
// gateway inventing one.
func (s *OpenAIGatewayService) applyOpenAISchemaRefCompatForGroup(c *gin.Context, body []byte) []byte {
	if s == nil || s.cfg == nil {
		return body
	}
	gw := s.cfg.Gateway
	if len(gw.OpenAIStripSchemaRefSiblingsGroupIDs) == 0 && len(gw.OpenAIFixSchemaRefURIGroupIDs) == 0 {
		return body
	}
	groupID := getOpenAIGroupIDFromContext(c)
	if groupID <= 0 {
		return body
	}
	strip := slices.Contains(gw.OpenAIStripSchemaRefSiblingsGroupIDs, groupID)
	fix := slices.Contains(gw.OpenAIFixSchemaRefURIGroupIDs, groupID)
	if !strip && !fix {
		return body
	}
	sanitized, changed, err := sanitizeOpenAISchemaRefs(body, strip, fix)
	if err != nil {
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Skip $ref compatibility pass for group %d: %v", groupID, err)
		return body
	}
	if !changed {
		return body
	}
	logger.LegacyPrintf("service.openai_gateway",
		"[OpenAI] Applied $ref compatibility to request schema (group: %d, strip_siblings: %t, fix_uri: %t)", groupID, strip, fix)
	return sanitized
}

// sanitizeOpenAISchemaRefs runs the selected $ref compatibility passes over
// function tool parameters and json_schema response formats in one parse.
func sanitizeOpenAISchemaRefs(body []byte, stripSiblings, fixURIs bool) ([]byte, bool, error) {
	if len(body) == 0 || (!stripSiblings && !fixURIs) || !bytes.Contains(body, []byte("$ref")) {
		return body, false, nil
	}
	return sanitizeOpenAIResponsesToolSchemas(body, openAIResponsesToolSchemaOptions{
		stripRefAnnotationSiblings:   stripSiblings,
		fixInvalidRefURIs:            fixURIs,
		includeResponseFormatSchemas: true,
	})
}

// fixOpenAISchemaRefURIs repairs $ref values that are not URI references.
func fixOpenAISchemaRefURIs(body []byte) ([]byte, bool, error) {
	return sanitizeOpenAISchemaRefs(body, false, true)
}
