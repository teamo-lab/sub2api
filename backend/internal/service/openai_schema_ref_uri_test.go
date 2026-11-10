package service

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 11-10 nova（sv 分组 6）的真实报错：Codex 挂的 MCP 工具参数里带了 Springfox 生成的泛型名，
// Zero 和 codebot 都回 400 invalid_function_parameters:
// "#/components/schemas/AbstractView«object»" is not a "uri-reference"。

func TestFixOpenAISchemaRefURIsEncodesResolvableRef(t *testing.T) {
	body := []byte(`{"model":"gpt-6.1-sol","tools":[{"type":"function","name":"mcp__BestBuildServ__savePackageTaskUsingPOST","parameters":{"type":"object","properties":{"view":{"$ref":"#/components/schemas/AbstractView«object»"},"name":{"type":"string"}},"components":{"schemas":{"AbstractView«object»":{"type":"object","properties":{"id":{"type":"string"}}}}}}}]}`)
	got, changed, err := fixOpenAISchemaRefURIs(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"model":"gpt-6.1-sol","tools":[{"type":"function","name":"mcp__BestBuildServ__savePackageTaskUsingPOST","parameters":{"type":"object","properties":{"view":{"$ref":"#/components/schemas/AbstractView%C2%ABobject%C2%BB"},"name":{"type":"string"}},"components":{"schemas":{"AbstractView«object»":{"type":"object","properties":{"id":{"type":"string"}}}}}}}]}`, string(got))
	// The definition keys themselves are data, not refs: untouched.
	require.Contains(t, string(got), `"AbstractView«object»":{`)
}

func TestFixOpenAISchemaRefURIsDropsDanglingRef(t *testing.T) {
	// MCP servers built from Swagger often forward the ref without the components it points to.
	body := []byte(`{"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"view":{"description":"the view","$ref":"#/components/schemas/AbstractView«object»"},"list":{"type":"array","items":{"$ref":"#/definitions/Page«List«User»»"}}}}}]}`)
	got, changed, err := fixOpenAISchemaRefURIs(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"view":{"description":"the view"},"list":{"type":"array","items":{}}}}}]}`, string(got))
}

func TestFixOpenAISchemaRefURIsChatCompletionsAndResponseFormat(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"a":{"$ref":"#/$defs/Result«Order»"}},"$defs":{"Result«Order»":{"type":"string"}}}}}],"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object","properties":{"b":{"$ref":"#/definitions/Map«string,int»"}},"definitions":{"Map«string,int»":{"type":"object"}}}}}}`)
	got, changed, err := fixOpenAISchemaRefURIs(body)
	require.NoError(t, err)
	require.True(t, changed)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(got, &doc))
	require.Contains(t, string(got), `"$ref":"#/$defs/Result%C2%ABOrder%C2%BB"`)
	require.Contains(t, string(got), `"$ref":"#/definitions/Map%C2%ABstring,int%C2%BB"`)
}

func TestFixOpenAISchemaRefURIsLeavesValidAndNonSchemaRefsAlone(t *testing.T) {
	cases := map[string]string{
		"valid local ref":       `{"tools":[{"type":"function","name":"f","parameters":{"properties":{"a":{"$ref":"#/$defs/A"}},"$defs":{"A":{"type":"string"}}}}]}`,
		"already encoded":       `{"tools":[{"type":"function","name":"f","parameters":{"properties":{"a":{"$ref":"#/$defs/A%C2%ABB%C2%BB"}},"$defs":{"A«B»":{"type":"string"}}}}]}`,
		"valid external ref":    `{"tools":[{"type":"function","name":"f","parameters":{"properties":{"a":{"$ref":"https://example.com/schema.json#/A"}}}}]}`,
		"ref-like data default": `{"tools":[{"type":"function","name":"f","parameters":{"properties":{"a":{"type":"object","default":{"$ref":"#/x«y»"}}}}}]}`,
		"property named $ref":   `{"tools":[{"type":"function","name":"f","parameters":{"properties":{"$ref":{"type":"string"}}}}]}`,
		"message text":          `{"input":[{"role":"user","content":[{"type":"input_text","text":"{\"$ref\":\"#/a«b»\"}"}]}]}`,
		"no ref":                `{"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got, changed, err := fixOpenAISchemaRefURIs([]byte(raw))
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, raw, string(got))
		})
	}
}

func TestFixOpenAISchemaRefURIsWithStripSiblingsInOnePass(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"f","parameters":{"properties":{"a":{"description":"kept? no","$ref":"#/$defs/A«B»","title":"t"},"b":{"description":"kept: ref dropped","$ref":"#/$defs/Missing«X»"}},"$defs":{"A«B»":{"type":"string"}}}}]}`)
	got, changed, err := sanitizeOpenAISchemaRefs(body, true, true)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"tools":[{"type":"function","name":"f","parameters":{"properties":{"a":{"$ref":"#/$defs/A%C2%ABB%C2%BB"},"b":{"description":"kept: ref dropped"}},"$defs":{"A«B»":{"type":"string"}}}}]}`, string(got))
}

func TestOpenAIRefURIHelpers(t *testing.T) {
	for ref, want := range map[string]bool{
		"#/components/schemas/AbstractView«object»": false,
		"#/a b":                              false,
		"#/a#b":                              false,
		"#/a%zz":                             false,
		"#/$defs/A":                          true,
		"#/a%C2%AB":                          true,
		"#":                                  true,
		"other.json#/x":                      true,
		"http://[::1]/s.json#/definitions/x": true,
	} {
		require.Equal(t, want, openAIResponsesRefIsURIReference(ref), ref)
	}
	require.Equal(t, "#/a%20b%23c", openAIResponsesEncodeRefURI("#/a b#c"))
	require.Equal(t, "#/x%25y%C2%AB", openAIResponsesEncodeRefURI("#/x%y«"))
	require.Equal(t, "/x«y»", openAIResponsesPercentDecode("/x%C2%ABy%C2%BB"))
}

func TestApplyOpenAISchemaRefCompatForGroupHonorsFixAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"tools":[{"type":"function","name":"f","parameters":{"properties":{"a":{"$ref":"#/components/schemas/V«o»","description":"d"}}}}]}`)
	newCtx := func(groupID *int64) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Set("api_key", &APIKey{ID: 1, GroupID: groupID})
		return c
	}
	six, two := int64(6), int64(2)
	cfg := &config.Config{}
	cfg.Gateway.OpenAIFixSchemaRefURIGroupIDs = []int64{6}
	cfg.Gateway.OpenAIStripSchemaRefSiblingsGroupIDs = []int64{2}
	svc := &OpenAIGatewayService{cfg: cfg}

	// Group 6: fix only. The dangling ref is dropped; its description stays.
	require.JSONEq(t, `{"tools":[{"type":"function","name":"f","parameters":{"properties":{"a":{"description":"d"}}}}]}`,
		string(svc.applyOpenAISchemaRefCompatForGroup(newCtx(&six), body)))
	// Group 2: siblings strip only; the invalid ref is left for the upstream to report.
	require.JSONEq(t, `{"tools":[{"type":"function","name":"f","parameters":{"properties":{"a":{"$ref":"#/components/schemas/V«o»"}}}}]}`,
		string(svc.applyOpenAISchemaRefCompatForGroup(newCtx(&two), body)))
	require.Equal(t, string(body), string(svc.applyOpenAISchemaRefCompatForGroup(newCtx(nil), body)))
}
