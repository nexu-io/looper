package runtime

import (
	"context"
	"github.com/nexu-io/looper/internal/workerhost"
)

var privateFeishuClient = workerhost.PrivateFeishuClient
var postPrivateFeishuQuestion = workerhost.PostPrivateFeishuQuestion

func runPrivateFeishuHITLPoll(ctx context.Context, input defaultSchedulerTickInput) error {
	return workerhost.PollPrivateFeishu(ctx, workerhost.FeishuPollInput{Config: input.Config, Repos: input.Repos, NotificationGateway: input.NotificationGateway, Now: input.Now, AllowClaim: input.AllowClaim, OnHITLAnswerDelivered: input.OnHITLAnswerDelivered})
}
