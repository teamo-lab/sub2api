package service

import (
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestStripOpenAISchemaRefAnnotationSiblingsResponsesTextFormat(t *testing.T) {
	body := []byte(`{"model":"gpt-6.1-sol","text":{"format":{"type":"json_schema","name":"intro_brief","strict":true,"schema":{"type":"object","properties":{"title":{"type":"string"},"typography":{"$ref":"#/$defs/Typography", "description":"Font choices"},"palette":{"description":"Colors","$ref":"#/$defs/Palette"}},"$defs":{"Typography":{"type":"object","description":"kept: no $ref here"},"Palette":{"type":"object"}}}}}}`)
	got, changed, err := stripOpenAISchemaRefAnnotationSiblings(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"model":"gpt-6.1-sol","text":{"format":{"type":"json_schema","name":"intro_brief","strict":true,"schema":{"type":"object","properties":{"title":{"type":"string"},"typography":{"$ref":"#/$defs/Typography"},"palette":{"$ref":"#/$defs/Palette"}},"$defs":{"Typography":{"type":"object","description":"kept: no $ref here"},"Palette":{"type":"object"}}}}}}`, string(got))
	// Property order must survive: structured output follows schema order.
	require.Less(t, indexOf(got, `"title":{`), indexOf(got, `"typography"`))
	require.Less(t, indexOf(got, `"typography"`), indexOf(got, `"palette"`))
}

func TestStripOpenAISchemaRefAnnotationSiblingsChatResponseFormatAndTools(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object","properties":{"a":{"title":"A","$ref":"#/definitions/A","description":"d","examples":[1]}},"definitions":{"A":{"type":"string"}}}}},"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"b":{"type":"array","items":{"$ref":"#/$defs/B","default":{"$ref":"literal"}}}},"$defs":{"B":{"type":"string"}}}}}]}`)
	got, changed, err := stripOpenAISchemaRefAnnotationSiblings(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object","properties":{"a":{"$ref":"#/definitions/A"}},"definitions":{"A":{"type":"string"}}}}},"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"b":{"type":"array","items":{"$ref":"#/$defs/B"}}},"$defs":{"B":{"type":"string"}}}}}]}`, string(got))
}

func TestStripOpenAISchemaRefAnnotationSiblingsResponsesTools(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"c":{"$ref":"#/$defs/C","description":"d"}},"$defs":{"C":{"type":"integer"}}}}]}`)
	got, changed, err := stripOpenAISchemaRefAnnotationSiblings(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"c":{"$ref":"#/$defs/C"}},"$defs":{"C":{"type":"integer"}}}}]}`, string(got))
}

func TestStripOpenAISchemaRefAnnotationSiblingsLeavesSemanticsAlone(t *testing.T) {
	cases := map[string]string{
		// A property literally named "description" is a property, not an annotation.
		"property named description": `{"text":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"description":{"$ref":"#/$defs/D"}},"$defs":{"D":{"type":"string"}}}}}}`,
		// Validation keywords and $defs next to $ref change meaning; keep them.
		"validation siblings": `{"text":{"format":{"type":"json_schema","schema":{"$ref":"#/$defs/R","$defs":{"R":{"type":"object"}},"type":"object"}}}}`,
		// $ref outside any schema location is ordinary data.
		"input data":        `{"input":[{"role":"user","content":[{"type":"input_text","text":"{\"$ref\":\"x\",\"description\":\"y\"}"}]}],"metadata":{"$ref":"x","description":"y"}}`,
		"no ref at all":     `{"text":{"format":{"type":"json_schema","schema":{"type":"object","description":"d"}}}}`,
		"non schema format": `{"text":{"format":{"type":"text"}},"response_format":{"type":"json_object"}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got, changed, err := stripOpenAISchemaRefAnnotationSiblings([]byte(raw))
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, raw, string(got))
		})
	}
}

func TestStripOpenAISchemaRefAnnotationSiblingsKeepsWhitespaceValid(t *testing.T) {
	body := []byte("{\"text\": {\"format\": {\"type\": \"json_schema\", \"schema\": {\n  \"properties\": {\n    \"t\": {\n      \"description\": \"d\",\n      \"$ref\": \"#/$defs/T\",\n      \"title\": \"T\"\n    }\n  },\n  \"$defs\": {\"T\": {\"type\": \"string\"}}\n}}}}")
	got, changed, err := stripOpenAISchemaRefAnnotationSiblings(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"text":{"format":{"type":"json_schema","schema":{"properties":{"t":{"$ref":"#/$defs/T"}},"$defs":{"T":{"type":"string"}}}}}}`, string(got))
}

func TestStripOpenAISchemaRefSiblingsForGroupHonorsAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"text":{"format":{"type":"json_schema","schema":{"properties":{"t":{"$ref":"#/$defs/T","description":"d"}},"$defs":{"T":{"type":"string"}}}}}}`)
	newCtx := func(groupID *int64) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Set("api_key", &APIKey{ID: 1, GroupID: groupID})
		return c
	}
	two, three := int64(2), int64(3)
	cfg := &config.Config{}
	cfg.Gateway.OpenAIStripSchemaRefSiblingsGroupIDs = []int64{2}
	svc := &OpenAIGatewayService{cfg: cfg}

	require.NotContains(t, string(svc.stripOpenAISchemaRefSiblingsForGroup(newCtx(&two), body)), "description")
	require.Equal(t, string(body), string(svc.stripOpenAISchemaRefSiblingsForGroup(newCtx(&three), body)))
	require.Equal(t, string(body), string(svc.stripOpenAISchemaRefSiblingsForGroup(newCtx(nil), body)))

	disabled := &OpenAIGatewayService{cfg: &config.Config{}}
	require.Equal(t, string(body), string(disabled.stripOpenAISchemaRefSiblingsForGroup(newCtx(&two), body)))
}

func indexOf(body []byte, needle string) int {
	for i := 0; i+len(needle) <= len(body); i++ {
		if string(body[i:i+len(needle)]) == needle {
			return i
		}
	}
	return -1
}
