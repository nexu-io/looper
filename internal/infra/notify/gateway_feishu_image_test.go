package notify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadFeishuImageUsesMultipart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "option.png")
	if err := os.WriteFile(path, []byte("\x89PNG\r\n\x1a\nbody"), 0o600); err != nil {
		t.Fatal(err)
	}
	var contentType string
	gateway := NewGateway(Options{FeishuAppHTTP: func(_ context.Context, method, url string, headers map[string]string, body []byte) (int, []byte, error) {
		contentType = headers["Content-Type"]
		if !strings.Contains(url, "/open-apis/im/v1/images") || !strings.Contains(string(body), "image_type") {
			t.Fatalf("unexpected upload request: %s %s", url, body)
		}
		return 200, []byte(`{"code":0,"msg":"ok","data":{"image_key":"img_123"}}`), nil
	}})
	key, err := gateway.uploadFeishuImage(context.Background(), "token", path)
	if err != nil || key != "img_123" || !strings.HasPrefix(contentType, "multipart/form-data;") {
		t.Fatalf("upload = %q, %v, contentType=%q", key, err, contentType)
	}
}

func TestPostFeishuMessageCarriesUUID(t *testing.T) {
	var requestBody string
	gateway := NewGateway(Options{FeishuAppHTTP: func(_ context.Context, method, url string, headers map[string]string, body []byte) (int, []byte, error) {
		requestBody = string(body)
		return 200, []byte(`{"code":0,"msg":"ok","data":{"message_id":"om_1"}}`), nil
	}})
	_, err := gateway.postFeishuAppMessageWithUUID(context.Background(), "token", "chat", "root", "image", `{"image_key":"img"}`, "stable-uuid")
	if err != nil || !strings.Contains(requestBody, `"uuid":"stable-uuid"`) {
		t.Fatalf("body=%s err=%v", requestBody, err)
	}
}
