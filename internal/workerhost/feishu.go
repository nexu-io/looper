package workerhost

import (
	"context"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/infra/feishucli"
	"github.com/nexu-io/looper/internal/infra/notify"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func PrivateFeishuClient(cfg *config.Config) *feishucli.Client {
	if cfg == nil || !cfg.HITL.Enabled || !strings.EqualFold(cfg.HITL.AnswerTransport, "feishu") || cfg.HITL.Feishu == nil {
		return nil
	}
	return &feishucli.Client{Config: *cfg.HITL.Feishu}
}

func PostPrivateFeishuQuestion(ctx context.Context, cfg *config.Config, repos *storage.Repositories, gateway *notify.Gateway, loopID string) error {
	client := PrivateFeishuClient(cfg)
	if client == nil {
		return nil
	}
	unlock := loops.LockLoopRequeue(loopID)
	defer unlock()
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return err
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || loop.Status != "awaiting_human" || ask.Transport != "feishu" || ask.QuestionMessageID != "" {
		return nil
	}
	return gateway.SendHITLAsk(ctx, notify.HITLAskCard{
		AskedAt: ask.AskedAt, Transport: "feishu", ProjectID: loop.ProjectID, LoopID: loop.ID, LoopSeq: loop.Seq,
		Question: ask.Question, Options: ask.Options,
		Recommendation: ask.Recommendation, RecommendedOption: ask.RecommendedOption,
		Consequences: ask.Consequences, Confidence: ask.Confidence,
	})
}

func PollPrivateFeishu(ctx context.Context, input FeishuPollInput) error {
	client := PrivateFeishuClient(input.Config)
	if client == nil || input.Repos == nil {
		return nil
	}
	records, err := input.Repos.Loops.List(ctx)
	if err != nil {
		return err
	}
	verified := false
	for _, loop := range records {
		ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
		if loop.Status != "awaiting_human" || !ok || ask.Transport != "feishu" || ask.Status != "awaiting" {
			continue
		}
		if input.AllowClaim != nil {
			if err := input.AllowClaim(); err != nil {
				return err
			}
		}
		if ask.QuestionMessageID == "" {
			if err := PostPrivateFeishuQuestion(ctx, input.Config, input.Repos, input.NotificationGateway, loop.ID); err != nil {
				return err
			}
			continue
		}
		if !verified {
			if err := client.VerifyPrivateChat(ctx); err != nil {
				return err
			}
			verified = true
		}
		root, err := input.Repos.FeishuThreads.RootByLoop(ctx, loop.ID)
		if err != nil {
			return err
		}
		messages, err := client.ThreadMessages(ctx, root)
		if err != nil {
			return err
		}
		answer := feishucli.ReplyAfterQuestion(messages, client.Config, ask.QuestionMessageID)
		if answer != "" {
			nowISO := eventlog.FormatJavaScriptISOString(input.Now().UTC())
			if err := loops.DeliverHITLAnswerForQuestion(ctx, input.Repos, nowISO, loop.ID, answer, ask.QuestionMessageID); err != nil {
				return err
			}
			if input.OnHITLAnswerDelivered != nil {
				input.OnHITLAnswerDelivered(ctx, loop.ID, answer)
			}
		}
	}
	return nil
}

type FeishuPollInput struct {
	Config                *config.Config
	Repos                 *storage.Repositories
	NotificationGateway   *notify.Gateway
	Now                   func() time.Time
	AllowClaim            func() error
	OnHITLAnswerDelivered func(context.Context, string, string)
}
