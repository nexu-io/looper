package feishucli

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestAppHTTPKeepsOriginalImageBytesAndRejectsOtherChat(t *testing.T) {
	c := Client{Config: config.HITLFeishuConfig{ChatID: "oc_test", OwnerOpenID: "ou_owner"}}
	uploaded := false
	c.Run = func(_ context.Context, _ string, input []byte, args ...string) ([]byte, error) {
		switch strings.Join(args[:2], " ") {
		case "whoami --as":
			return []byte(`{"appId":"cli_bot","available":true}`), nil
		case "im +chat-members-list":
			return []byte(`{"ok":true,"data":{"users":[{"member_id":"ou_owner"}],"bots":[{"app_id":"cli_bot"}]}}`), nil
		case "api POST":
			if !bytes.Equal(input, []byte("original image bytes")) || !strings.Contains(strings.Join(args, " "), "--file image=- --as bot") {
				t.Fatalf("wrong upload: %v %q", args, input)
			}
			uploaded = true
			return []byte(`{"ok":true,"data":{"image_key":"img_original"}}`), nil
		default:
			t.Fatalf("unexpected write: %v", args)
			return nil, nil
		}
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, _ := form.CreateFormFile("image", "preview.png")
	_, _ = part.Write([]byte("original image bytes"))
	_ = form.Close()
	status, result, err := c.AppHTTP(context.Background(), http.MethodPost, "https://open.feishu.cn/open-apis/im/v1/images", map[string]string{"Content-Type": form.FormDataContentType()}, body.Bytes())
	if err != nil || status != 200 || !uploaded || !strings.Contains(string(result), `"image_key":"img_original"`) {
		t.Fatalf("upload: %d %s %v", status, result, err)
	}
	if _, _, err := c.AppHTTP(context.Background(), http.MethodPost, "https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type=chat_id", nil, []byte(`{"receive_id":"oc_big_group","msg_type":"interactive","content":"{}"}`)); err == nil {
		t.Fatal("accepted another chat")
	}
}
