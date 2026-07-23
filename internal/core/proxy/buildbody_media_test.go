package proxy

import (
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
)

func TestBuildBody_MediaRoleFromTemplate(t *testing.T) {
	tests := []struct {
		name     string
		template string
		version  string
		wantKey  string
	}{
		{"input images", `{"params":{"input_images":[]}}`, "v2", "input_images"},
		{"medias", `{"params":{"medias":[]}}`, "v2", "medias"},
		{"input image", `{"params":{"input_image":null}}`, "v1-hyphen", "input_image"},
		{"v2 fallback", `{"params":{}}`, "v2", "input_images"},
		{"v1 fallback", `{"params":{}}`, "v1-hyphen", "input_image"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := &domain.ModelSpec{Version: tc.version, ExampleBodyJSON: tc.template}
			req := GenerationRequest{Media: &MediaInput{PreUploadedID: "media-1", Type: "image", URL: "https://cdn/x.jpg"}}
			body := buildBody(spec, req, false)
			params := body["params"].(map[string]any)
			if _, ok := params[tc.wantKey]; !ok {
				t.Fatalf("params missing %q: %#v", tc.wantKey, params)
			}
		})
	}
}

func TestBuildBody_URLOnlyMediaIsInjected(t *testing.T) {
	spec := &domain.ModelSpec{Version: "v2", ExampleBodyJSON: `{"params":{"input_images":[]}}`}
	req := GenerationRequest{Media: &MediaInput{Type: "image", URL: "https://cdn/x.jpg"}}
	body := buildBody(spec, req, false)
	params := body["params"].(map[string]any)
	items := params["input_images"].([]any)
	media := items[0].(map[string]any)
	if media["url"] != "https://cdn/x.jpg" {
		t.Fatalf("url = %v", media["url"])
	}
}
