package apicompat

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Cache boundaries belong to content blocks, not their concatenated text.
// Keep the unmarked legacy conversion unchanged.
func hasCacheBoundary(raw json.RawMessage) bool {
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		var part map[string]json.RawMessage
		if json.Unmarshal(raw, &part) != nil {
			return false
		}
		parts = []map[string]json.RawMessage{part}
	}
	for _, p := range parts {
		if v := p["prompt_cache_breakpoint"]; len(v) > 0 && !bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return true
		}
	}
	return false
}

func convertCacheMarkedContent(raw json.RawMessage, responses bool, role string) (json.RawMessage, error) {
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		var part map[string]json.RawMessage
		if err := json.Unmarshal(raw, &part); err != nil {
			return nil, err
		}
		parts = []map[string]json.RawMessage{part}
	}
	for _, p := range parts {
		kind := rawString(p["type"])
		switch kind {
		case "text", "input_text", "output_text", "":
			target := "text"
			if responses {
				target = "input_text"
				if role == "assistant" {
					target = "output_text"
				}
			}
			p["type"], _ = json.Marshal(target)
		case "input_image", "image_url":
			if !responses && role != "user" {
				return nil, fmt.Errorf("cache-marked multimodal %s content requires a Responses-capable upstream", role)
			}
			p["type"], _ = json.Marshal("image_url")
			if responses {
				p["type"], _ = json.Marshal("input_image")
				var image map[string]json.RawMessage
				if json.Unmarshal(p["image_url"], &image) == nil {
					p["image_url"] = image["url"]
					if d := image["detail"]; len(d) > 0 {
						p["detail"] = d
					}
				}
			} else {
				var url string
				if json.Unmarshal(p["image_url"], &url) == nil {
					image := map[string]json.RawMessage{"url": p["image_url"]}
					if d := p["detail"]; len(d) > 0 {
						image["detail"] = d
						delete(p, "detail")
					}
					p["image_url"], _ = json.Marshal(image)
				}
			}
		case "input_file", "file":
			if !responses && role != "user" {
				return nil, fmt.Errorf("cache-marked multimodal %s content requires a Responses-capable upstream", role)
			}
			if responses {
				p["type"], _ = json.Marshal("input_file")
				var file map[string]json.RawMessage
				if json.Unmarshal(p["file"], &file) == nil {
					for k, v := range file {
						p[k] = v
					}
					delete(p, "file")
				}
			} else {
				p["type"], _ = json.Marshal("file")
				file := map[string]json.RawMessage{}
				for _, k := range []string{"filename", "file_data", "file_id"} {
					if v := p[k]; len(v) > 0 {
						file[k] = v
						delete(p, k)
					}
				}
				p["file"], _ = json.Marshal(file)
			}
		default:
			return nil, fmt.Errorf("cannot preserve cache boundary in unsupported content type %q", kind)
		}
	}
	return json.Marshal(parts)
}
